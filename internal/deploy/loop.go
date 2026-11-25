package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
	"github.com/robertjndw/oakestra-openai-api/internal/store"
)

// unhealthyAfter is how many probes in a row must fail before a ready
// engine is taken out of rotation. One slow answer under load is normal.
const unhealthyAfter = 3

// tick runs one step for every deployment that is not failed.
func (m *Manager) tick(ctx context.Context) {
	m.tickMu.Lock()
	defer m.tickMu.Unlock()

	rows, err := m.queryRows(ctx, `WHERE d.state != ?`, StateFailed)
	if err != nil {
		m.log.Error("deploy: tick", "err", err)
		return
	}
	// Engines that take traffic are probed first: their steps only talk to
	// the engine, so a slow Oakestra cannot delay taking a dead one out of
	// rotation.
	isServing := func(r row) bool { return r.State == StateReady || r.State == StateUnhealthy }
	serving := make(map[string]bool)
	for _, first := range []bool{true, false} {
		for _, r := range rows {
			if ctx.Err() != nil {
				return
			}
			if isServing(r) != first {
				continue
			}
			if first {
				serving[r.ID] = true
			}
			m.step(ctx, r)
		}
	}
	for id := range m.probeFailures {
		if !serving[id] {
			delete(m.probeFailures, id)
		}
	}
	m.publish(ctx, false)
}

func (m *Manager) step(ctx context.Context, r row) {
	switch r.State {
	case StatePending:
		m.stepPending(ctx, r)
	case StateScheduling:
		m.stepScheduling(ctx, r)
	case StateStarting:
		m.stepStarting(ctx, r)
	case StateReady, StateUnhealthy:
		m.stepServing(ctx, r)
	case StateDeleting:
		m.stepDeleting(ctx, r)
	case StateFailed:
	default:
		m.log.Warn("deploy: deployment in unknown state", "deployment", r.ID, "state", r.State)
	}
}

// stepPending gets the service created and one instance requested. The
// service id is stored before DeployInstance, and a row without one first
// looks for a service with its deterministic name, so a crash at any point
// neither loses nor duplicates the service.
func (m *Manager) stepPending(ctx context.Context, r row) {
	mod, err := m.GetModel(ctx, r.ModelID)
	if err != nil {
		m.note(ctx, r, err.Error())
		return
	}
	var svc *oakestra.Service
	if r.serviceID == "" {
		appID, err := m.application(ctx)
		if err != nil {
			m.note(ctx, r, err.Error())
			return
		}
		services, err := m.oak.ListServices(ctx, appID)
		if errors.Is(err, oakestra.ErrNotFound) {
			// The application was deleted behind our back; ensure it again.
			m.forgetApplication(ctx)
		}
		if err != nil {
			m.note(ctx, r, err.Error())
			return
		}
		for i := range services {
			if services[i].MicroserviceName == r.serviceName {
				svc = &services[i]
				m.log.Info("deploy: adopting existing service", "deployment", r.ID, "service", svc.ID)
				break
			}
		}
		if svc == nil {
			ms, err := oakestra.BuildMicroservice(m.serviceSpec(r, mod))
			if err != nil {
				m.transition(ctx, r, StateFailed, err.Error())
				return
			}
			id, err := m.oak.CreateService(ctx, appID, ms)
			if err != nil {
				if errors.Is(err, oakestra.ErrInvalid) {
					m.transition(ctx, r, StateFailed, err.Error())
				} else {
					m.note(ctx, r, err.Error())
				}
				return
			}
			svc = &oakestra.Service{ID: id}
		}
		if err := m.setServiceID(ctx, r.ID, svc.ID); err != nil {
			m.note(ctx, r, err.Error())
			return
		}
		r.serviceID = svc.ID
	} else {
		s, err := m.oak.GetService(ctx, r.serviceID)
		if errors.Is(err, oakestra.ErrNotFound) {
			// Removed in Oakestra since; the next tick creates it anew.
			if err := m.setServiceID(ctx, r.ID, ""); err != nil {
				m.note(ctx, r, err.Error())
				return
			}
			m.note(ctx, r, fmt.Sprintf("service %s disappeared from Oakestra; creating it again", r.serviceID))
			return
		}
		if err != nil {
			m.note(ctx, r, err.Error())
			return
		}
		svc = &s
	}

	// An instance or a scheduling verdict means DeployInstance already ran
	// before a crash; asking again would place a second engine.
	if len(svc.InstanceList) == 0 && !isFailure(svc.Status) {
		if err := m.oak.DeployInstance(ctx, r.serviceID); err != nil {
			m.note(ctx, r, err.Error())
			return
		}
	}
	m.transition(ctx, r, StateScheduling, "")
}

func (m *Manager) stepScheduling(ctx context.Context, r row) {
	svc, err := m.oak.GetService(ctx, r.serviceID)
	if errors.Is(err, oakestra.ErrNotFound) {
		m.transition(ctx, r, StateFailed, fmt.Sprintf("service %s no longer exists in Oakestra", r.serviceID))
		return
	}
	if err != nil {
		m.note(ctx, r, err.Error())
		return
	}
	if inst := latestInstance(svc); inst != nil && inst.Status == oakestra.StatusRunning {
		m.transition(ctx, r, StateStarting, "")
		return
	}
	if reason := failureReason(svc); reason != "" {
		m.transition(ctx, r, StateFailed, reason)
		return
	}
	if waited := m.now().Sub(r.stateSince); waited > m.cfg.ScheduleTimeout {
		m.transition(ctx, r, StateFailed, fmt.Sprintf("Oakestra did not report the engine running within %s (status %q)",
			m.cfg.ScheduleTimeout, serviceStatus(svc)))
		return
	}
	m.note(ctx, r, "")
}

func (m *Manager) stepStarting(ctx context.Context, r row) {
	// Oakestra is only asked to fail fast on a dead container. When it is
	// unreachable the probe still decides, since the engine may be fine.
	svc, err := m.oak.GetService(ctx, r.serviceID)
	switch {
	case errors.Is(err, oakestra.ErrNotFound):
		m.transition(ctx, r, StateFailed, fmt.Sprintf("service %s no longer exists in Oakestra", r.serviceID))
		return
	case err == nil:
		if reason := failureReason(svc); reason != "" {
			m.transition(ctx, r, StateFailed, reason)
			return
		}
	}
	perr := m.probeEngine(ctx, r)
	if perr == nil {
		m.transition(ctx, r, StateReady, "")
		return
	}
	if waited := m.now().Sub(r.stateSince); waited > m.cfg.StartupTimeout {
		m.transition(ctx, r, StateFailed, fmt.Sprintf("engine did not become ready within %s: %v", m.cfg.StartupTimeout, perr))
		return
	}
	m.note(ctx, r, perr.Error())
}

// stepServing probes a ready or unhealthy engine. Neither state deletes or
// redeploys anything; unhealthy only takes the engine out of routing.
func (m *Manager) stepServing(ctx context.Context, r row) {
	err := m.probeEngine(ctx, r)
	if err == nil {
		delete(m.probeFailures, r.ID)
		if r.State == StateUnhealthy {
			m.transition(ctx, r, StateReady, "")
		} else {
			m.note(ctx, r, "")
		}
		return
	}
	m.probeFailures[r.ID]++
	if r.State == StateReady && m.probeFailures[r.ID] >= unhealthyAfter {
		m.transition(ctx, r, StateUnhealthy, err.Error())
		return
	}
	m.note(ctx, r, err.Error())
}

func (m *Manager) stepDeleting(ctx context.Context, r row) {
	ids := []string{r.serviceID}
	if r.serviceID == "" {
		// CreateService may have succeeded without the id reaching the row
		// (a timeout after the server did the work, or a crash). Look the
		// service up by its deterministic name so it is not left behind.
		found, err := m.servicesNamed(ctx, r.serviceName)
		if err != nil {
			m.note(ctx, r, err.Error())
			return
		}
		ids = found
	}
	for _, id := range ids {
		if err := m.oak.DeleteService(ctx, id); err != nil && !errors.Is(err, oakestra.ErrNotFound) {
			m.note(ctx, r, err.Error())
			return
		}
	}
	_, err := m.db.ExecContext(ctx, `DELETE FROM deployments WHERE id = ? AND state = ?`, r.ID, StateDeleting)
	if err != nil {
		m.log.Error("deploy: delete deployment row", "deployment", r.ID, "err", err)
		return
	}
	delete(m.probeFailures, r.ID)
	m.log.Info("deploy: deployment removed", "deployment", r.ID, "model", r.ModelName)
}

// servicesNamed returns the ids of the application's services called name.
// It never creates the application: as long as none was ever ensured, no
// service can exist, and a deployment removed before its first step goes
// away without needing Oakestra.
func (m *Manager) servicesNamed(ctx context.Context, name string) ([]string, error) {
	appID := m.appID
	if appID == "" {
		id, _, err := store.GetSettingAs[string](ctx, m.db, store.SettingOakestraAppID)
		if err != nil {
			return nil, err
		}
		appID = id
	}
	if appID == "" {
		return nil, nil
	}
	services, err := m.oak.ListServices(ctx, appID)
	if errors.Is(err, oakestra.ErrNotFound) {
		m.forgetApplication(ctx)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, s := range services {
		if s.MicroserviceName == name {
			ids = append(ids, s.ID)
		}
	}
	return ids, nil
}

func (m *Manager) serviceSpec(r row, mod Model) oakestra.ServiceSpec {
	return oakestra.ServiceSpec{
		Name:          r.serviceName,
		Namespace:     m.cfg.AppNS,
		Image:         mod.EngineImage,
		Command:       EngineArgv(mod, m.cfg.ModelMountPath, r.engineKey),
		HostPort:      r.hostPort,
		ContainerPort: mod.ContainerPort,
		MemoryMB:      mod.MemoryMB,
		VGPUs:         mod.VGPUs,
		Cluster:       r.Cluster,
		Node:          r.Node,
		Volumes: []oakestra.Volume{{
			// Unique per deployment and never "pvc-..." (see store.NewID),
			// which the modelpack driver would treat as a dynamic volume.
			VolumeID:  r.ID,
			CSIDriver: m.cfg.ModelCSIDriver,
			MountPath: m.cfg.ModelMountPath,
			Config:    map[string]string{m.cfg.ModelCSIRefKey: mod.Reference},
		}},
	}
}

// application returns the id of the Oakestra application that owns every
// engine service, creating the application on first use.
func (m *Manager) application(ctx context.Context) (string, error) {
	if m.appID != "" {
		return m.appID, nil
	}
	id, ok, err := store.GetSettingAs[string](ctx, m.db, store.SettingOakestraAppID)
	if err != nil {
		return "", err
	}
	if ok && id != "" {
		m.appID = id
		return id, nil
	}
	app, err := oakestra.NewApplication(m.cfg.AppName, m.cfg.AppNS, "LLM engines of the openai-api addon")
	if err != nil {
		return "", err
	}
	info, err := oakestra.EnsureApplication(ctx, m.oak, app)
	if err != nil {
		return "", err
	}
	if err := store.PutSetting(ctx, m.db, store.SettingOakestraAppID, info.ApplicationID); err != nil {
		return "", err
	}
	m.appID = info.ApplicationID
	return m.appID, nil
}

func (m *Manager) forgetApplication(ctx context.Context) {
	m.appID = ""
	if err := store.DeleteSetting(ctx, m.db, store.SettingOakestraAppID); err != nil {
		m.log.Error("deploy: forget application id", "err", err)
	}
}

// setServiceID is not guarded by state: a Delete that raced CreateService
// still needs the id to remove the service.
func (m *Manager) setServiceID(ctx context.Context, id, serviceID string) error {
	_, err := m.db.ExecContext(ctx, `UPDATE deployments SET service_id = ?, updated_at = ? WHERE id = ?`,
		serviceID, store.Millis(m.now()), id)
	if err != nil {
		return fmt.Errorf("deploy: store service id: %w", err)
	}
	return nil
}

// transition moves r to state to. The update is conditional on the state
// the step started from, so an admin Delete or Retry that happened during
// the step wins.
func (m *Manager) transition(ctx context.Context, r row, to State, errText string) {
	now := store.Millis(m.now())
	res, err := m.db.ExecContext(ctx,
		`UPDATE deployments SET state = ?, error = ?, state_since = ?, updated_at = ? WHERE id = ? AND state = ?`,
		to, errText, now, now, r.ID, r.State)
	if err != nil {
		m.log.Error("deploy: update deployment state", "deployment", r.ID, "to", to, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	level := slog.LevelInfo
	if to == StateFailed || to == StateUnhealthy {
		level = slog.LevelWarn
	}
	m.log.Log(ctx, level, "deploy: deployment state changed", "deployment", r.ID, "model", r.ModelName,
		"from", r.State, "to", to, "error", errText)
}

// note records the outcome of a step that did not change the state: the
// error text of a transient problem, or "" once things go fine again.
func (m *Manager) note(ctx context.Context, r row, errText string) {
	if r.Error == errText {
		return
	}
	_, err := m.db.ExecContext(ctx, `UPDATE deployments SET error = ?, updated_at = ? WHERE id = ? AND state = ?`,
		errText, store.Millis(m.now()), r.ID, r.State)
	if err != nil {
		m.log.Error("deploy: record deployment error", "deployment", r.ID, "err", err)
		return
	}
	if errText != "" {
		m.log.Debug("deploy: step did not complete", "deployment", r.ID, "state", r.State, "error", errText)
	}
}

// probeEngine checks /health and /v1/models with the engine key. vLLM
// answers /health before the model is registered, and /v1/models also
// proves the key is accepted.
func (m *Manager) probeEngine(ctx context.Context, r row) error {
	base := r.baseURL()
	for _, path := range []string{"/health", "/v1/models"} {
		if err := m.probeGet(ctx, base+path, r.engineKey); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) probeGet(ctx context.Context, url, key string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("probe %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := m.probe.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("probe %s: status %d", url, resp.StatusCode)
	}
	return nil
}

func latestInstance(s oakestra.Service) *oakestra.Instance {
	var latest *oakestra.Instance
	for i := range s.InstanceList {
		if latest == nil || s.InstanceList[i].InstanceNumber > latest.InstanceNumber {
			latest = &s.InstanceList[i]
		}
	}
	return latest
}

// failureReason returns why Oakestra gave up on the engine, or "". A
// rejected placement has no instance and the verdict on the service, a
// dead container shows on its instance.
func failureReason(s oakestra.Service) string {
	status, detail := s.Status, s.StatusDetail
	if inst := latestInstance(s); inst != nil {
		status, detail = inst.Status, inst.StatusDetail
	}
	if !isFailure(status) {
		return ""
	}
	if detail == "" {
		return status
	}
	return status + ": " + detail
}

func serviceStatus(s oakestra.Service) string {
	if inst := latestInstance(s); inst != nil {
		return inst.Status
	}
	return s.Status
}

// isFailure reports whether status means the engine will not come up
// without intervention. COMPLETED counts: an engine is never meant to exit.
func isFailure(status string) bool {
	switch status {
	case oakestra.StatusFailed, oakestra.StatusDead, oakestra.StatusCompleted, oakestra.StatusUndeployed,
		oakestra.StatusNoWorkerCapacity, oakestra.StatusNoQualifiedWorkerFound, oakestra.StatusNoNodeFound,
		oakestra.StatusTargetClusterNotFound, oakestra.StatusTargetClusterNotActive,
		oakestra.StatusTargetClusterNoCap, oakestra.StatusNoActiveClusterWithCap:
		return true
	}
	return false
}
