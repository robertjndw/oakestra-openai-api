package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/config"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra/oakestratest"
	"github.com/robertjndw/oakestra-openai-api/internal/store"
)

func TestHappyPath(t *testing.T) {
	h := newHarness(t, nil)
	mod := h.model("qwen")
	d := h.deploy(mod, "c1", "n1")
	if d.State != StatePending || d.ModelName != "qwen" {
		t.Fatalf("created %+v", d)
	}
	h.toReady(d.ID)

	r := h.row(d.ID)
	if r.Error != "" || r.hostPort != 18000 || r.nodeIP != "127.0.0.1" || !oakestra.ValidName(r.serviceName) {
		t.Errorf("row %+v", r)
	}
	svcs := h.fake.Services()
	if len(svcs) != 1 {
		t.Fatalf("services = %d, want 1", len(svcs))
	}
	sla := svcs[0].SLA
	if svcs[0].ID != r.serviceID || sla.MicroserviceName != r.serviceName {
		t.Errorf("service %s %s, row %s %s", svcs[0].ID, sla.MicroserviceName, r.serviceID, r.serviceName)
	}
	wantCmd := []string{"vllm", "serve", "/model", "--served-model-name", "qwen", "--port", "8000", "--api-key", r.engineKey}
	if !slices.Equal(sla.Cmd, wantCmd) {
		t.Errorf("cmd = %q, want %q", sla.Cmd, wantCmd)
	}
	wantVol := oakestra.Volume{VolumeID: d.ID, CSIDriver: oakestra.ModelpackCSIDriver, MountPath: "/model",
		Config: map[string]string{oakestra.ModelpackReferenceKey: mod.Reference}}
	if len(sla.Volumes) != 1 || !volumeEqual(sla.Volumes[0], wantVol) {
		t.Errorf("volumes = %+v, want %+v", sla.Volumes, wantVol)
	}
	if sla.Port != "18000:8000" || sla.Code != h.cfg.DefaultEngineImage || sla.Memory != 1024 {
		t.Errorf("port %q code %q memory %d", sla.Port, sla.Code, sla.Memory)
	}
	if len(sla.Constraints) != 1 || sla.Constraints[0].Type != oakestra.ConstraintDirect ||
		sla.Constraints[0].Cluster != "c1" || sla.Constraints[0].Node != "n1" {
		t.Errorf("constraints = %+v", sla.Constraints)
	}
	if id, ok, _ := store.GetSettingAs[string](t.Context(), h.db, store.SettingOakestraAppID); !ok || id != svcs[0].ApplicationID {
		t.Errorf("cached app id %q, want %q", id, svcs[0].ApplicationID)
	}

	// The secrets never reach JSON.
	b, _ := json.Marshal(h.row(d.ID).Deployment)
	if strings.Contains(string(b), r.engineKey) || strings.Contains(string(b), "port") || strings.Contains(string(b), r.serviceID) {
		t.Errorf("deployment JSON leaks internals: %s", b)
	}

	// Ready rows stay ready and nothing else happens.
	h.tick()
	h.wantState(d.ID, StateReady)
	if len(h.fake.Services()) != 1 {
		t.Error("a ready tick created services")
	}
	c, err := h.m.Counts(t.Context())
	if err != nil || c != (Counts{Models: 1, Deployments: 1, Ready: 1}) {
		t.Errorf("Counts = %+v, %v", c, err)
	}
	if err := h.m.CheckOakestra(t.Context()); err != nil {
		t.Errorf("CheckOakestra: %v", err)
	}
}

func volumeEqual(a, b oakestra.Volume) bool {
	return a.VolumeID == b.VolumeID && a.CSIDriver == b.CSIDriver && a.MountPath == b.MountPath &&
		len(a.Config) == len(b.Config) && a.Config[oakestra.ModelpackReferenceKey] == b.Config[oakestra.ModelpackReferenceKey]
}

func TestCreateDeploymentValidation(t *testing.T) {
	h := newHarness(t, nil)
	h.addNode("c1", oakestratest.NodeSpec{Name: "nocsi", CSIDrivers: []string{oakestra.HostpathCSIDriver}})
	h.addNode("c1", oakestratest.NodeSpec{Name: "idle"})
	if _, err := h.fake.UpdateNode("idle", oakestratest.NodeUpdate{Active: new(false)}); err != nil {
		t.Fatal(err)
	}
	mod := h.model("qwen")
	ctx := t.Context()

	_, err := h.m.CreateDeployment(ctx, mod.ID, "c1", "nocsi")
	assertStatus(t, err, http.StatusUnprocessableEntity)
	if !strings.Contains(err.Error(), oakestra.ModelpackCSIDriver) {
		t.Errorf("error does not name the driver: %v", err)
	}
	_, err = h.m.CreateDeployment(ctx, mod.ID, "c1", "idle")
	assertStatus(t, err, http.StatusUnprocessableEntity)
	_, err = h.m.CreateDeployment(ctx, mod.ID, "c1", "missing")
	assertStatus(t, err, http.StatusUnprocessableEntity)
	_, err = h.m.CreateDeployment(ctx, mod.ID, "c2", "n1")
	assertStatus(t, err, http.StatusUnprocessableEntity)
	_, err = h.m.CreateDeployment(ctx, "m_missing", "c1", "n1")
	assertStatus(t, err, http.StatusUnprocessableEntity)

	h.deploy(mod, "c1", "n1")
	_, err = h.m.CreateDeployment(ctx, mod.ID, "c1", "n1")
	assertStatus(t, err, http.StatusConflict)

	h.down.Store(true)
	_, err = h.m.CreateDeployment(ctx, h.model("llama").ID, "c1", "n1")
	assertStatus(t, err, http.StatusServiceUnavailable)

	list, err := h.m.ListDeployments(ctx)
	if err != nil || len(list) != 1 {
		t.Errorf("ListDeployments = %+v, %v", list, err)
	}
}

// Create checks the driver, but the node can lose it before Oakestra
// schedules; the fake then rejects the placement like cpumemfit does.
func TestNodeLosesDriverAfterCreate(t *testing.T) {
	h := newHarness(t, nil)
	d := h.deploy(h.model("qwen"), "c1", "n1")
	if _, err := h.fake.UpdateNode("n1", oakestratest.NodeUpdate{CSIDrivers: &[]string{oakestra.HostpathCSIDriver}}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.wantState(d.ID, StateScheduling)
	h.tick()
	r := h.wantState(d.ID, StateFailed)
	if !strings.Contains(r.Error, "does not have required CSI driver "+oakestra.ModelpackCSIDriver) ||
		!strings.Contains(r.Error, oakestra.StatusNoActiveClusterWithCap) {
		t.Errorf("error = %q", r.Error)
	}
	// Failed rows are left alone.
	h.tick()
	h.wantState(d.ID, StateFailed)
	if len(h.fake.Services()) != 1 {
		t.Error("failed deployment's service was touched")
	}

	// Retry after fixing the node removes the old service and starts over.
	if _, err := h.fake.UpdateNode("n1", oakestratest.NodeUpdate{CSIDrivers: &oakestratest.DefaultCSIDrivers}); err != nil {
		t.Fatal(err)
	}
	old := r.serviceID
	got, err := h.m.RetryDeployment(t.Context(), d.ID)
	if err != nil || got.State != StatePending || got.Error != "" {
		t.Fatalf("Retry = %+v, %v", got, err)
	}
	if _, ok := h.fake.Service(old); ok {
		t.Error("old service survived the retry")
	}
	h.toReady(d.ID)
	if len(h.fake.Services()) != 1 {
		t.Errorf("services = %d after retry, want 1", len(h.fake.Services()))
	}
	_, err = h.m.RetryDeployment(t.Context(), d.ID)
	assertStatus(t, err, http.StatusConflict)
	_, err = h.m.RetryDeployment(t.Context(), "d_missing")
	assertStatus(t, err, http.StatusNotFound)
}

// A crash after CreateService but before the id was stored leaves a
// service with the deterministic name and a pending row without id.
func TestAdoptsServiceAfterCrash(t *testing.T) {
	h := newHarness(t, nil)
	mod := h.model("qwen")
	d := h.deploy(mod, "c1", "n1")
	r := h.row(d.ID)

	appID, err := h.m.application(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ms, err := oakestra.BuildMicroservice(h.m.serviceSpec(r, mod))
	if err != nil {
		t.Fatal(err)
	}
	svcID, err := h.oak.CreateService(t.Context(), appID, ms)
	if err != nil {
		t.Fatal(err)
	}

	h.tick()
	r = h.wantState(d.ID, StateScheduling)
	if r.serviceID != svcID {
		t.Errorf("service id = %q, want adopted %q", r.serviceID, svcID)
	}
	if n := len(h.fake.Services()); n != 1 {
		t.Fatalf("services = %d, want 1 (adopted, not duplicated)", n)
	}
	h.tick()
	h.tick()
	h.wantState(d.ID, StateReady)
}

// A crash after DeployInstance but before the state moved on must not
// place a second instance.
func TestPendingWithDeployedServiceDoesNotRedeploy(t *testing.T) {
	h := newHarness(t, nil)
	d := h.deploy(h.model("qwen"), "c1", "n1")
	h.tick()
	r := h.wantState(d.ID, StateScheduling)
	if _, err := h.db.ExecContext(t.Context(), `UPDATE deployments SET state = 'pending' WHERE id = ?`, d.ID); err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.wantState(d.ID, StateScheduling)
	svc, _ := h.fake.Service(r.serviceID)
	if len(svc.InstanceList) != 1 {
		t.Errorf("instances = %d, want 1", len(svc.InstanceList))
	}
}

func TestOakestraUnreachable(t *testing.T) {
	h := newHarness(t, nil)
	d := h.deploy(h.model("qwen"), "c1", "n1")

	h.down.Store(true)
	h.tick()
	h.tick()
	r := h.wantState(d.ID, StatePending)
	if r.Error == "" {
		t.Error("no error recorded while Oakestra is down")
	}

	h.down.Store(false)
	h.fake.Fail(oakestratest.Failure{Method: http.MethodPost, Path: "/api/service/", Status: http.StatusInternalServerError, Message: "boom", Times: 1})
	h.tick()
	r = h.wantState(d.ID, StatePending)
	if !strings.Contains(r.Error, "boom") {
		t.Errorf("error = %q", r.Error)
	}

	h.tick()
	h.wantState(d.ID, StateScheduling)
	if r := h.row(d.ID); r.Error != "" {
		t.Errorf("error not cleared: %q", r.Error)
	}

	// Down while scheduling: no timeout check, no failure.
	h.down.Store(true)
	h.advance(time.Hour)
	h.tick()
	h.wantState(d.ID, StateScheduling)
	h.down.Store(false)
	h.tick()
	h.wantState(d.ID, StateStarting)

	// Down while starting: the probe still decides.
	h.down.Store(true)
	h.tick()
	h.wantState(d.ID, StateReady)
	if n := len(h.fake.Services()); n != 1 {
		t.Errorf("services = %d, want 1", n)
	}
}

func TestDeleteWhileUnreachable(t *testing.T) {
	h := newHarness(t, nil)
	d := h.deploy(h.model("qwen"), "c1", "n1")
	h.toReady(d.ID)

	if err := h.m.DeleteDeployment(t.Context(), d.ID); err != nil {
		t.Fatal(err)
	}
	h.wantState(d.ID, StateDeleting)
	if err := h.m.DeleteDeployment(t.Context(), d.ID); err != nil {
		t.Errorf("second delete: %v", err)
	}

	h.down.Store(true)
	h.tick()
	h.tick()
	if r := h.wantState(d.ID, StateDeleting); r.Error == "" {
		t.Error("no error recorded")
	}
	h.down.Store(false)
	h.tick()
	if !h.gone(d.ID) {
		t.Fatal("row survived")
	}
	if n := len(h.fake.Services()); n != 0 {
		t.Errorf("services = %d, want 0", n)
	}
	assertStatus(t, h.m.DeleteDeployment(t.Context(), d.ID), http.StatusNotFound)
}

func TestDeleteBeforeServiceExists(t *testing.T) {
	h := newHarness(t, nil)
	d := h.deploy(h.model("qwen"), "c1", "n1")
	if err := h.m.DeleteDeployment(t.Context(), d.ID); err != nil {
		t.Fatal(err)
	}
	h.down.Store(true)
	h.tick()
	if !h.gone(d.ID) {
		t.Fatal("row without service survived")
	}
}

func TestDeleteServiceAlreadyGone(t *testing.T) {
	h := newHarness(t, nil)
	d := h.deploy(h.model("qwen"), "c1", "n1")
	h.toReady(d.ID)
	if err := h.oak.DeleteService(t.Context(), h.row(d.ID).serviceID); err != nil {
		t.Fatal(err)
	}
	if err := h.m.DeleteDeployment(t.Context(), d.ID); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if !h.gone(d.ID) {
		t.Fatal("row survived although its service was already gone")
	}
}

func TestPortAllocation(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.EnginePortRange = config.PortRange{Lo: 18000, Hi: 18001} })
	h.addNode("c1", oakestratest.NodeSpec{Name: "n2"}) // shares 127.0.0.1 with n1
	h.addNode("c1", oakestratest.NodeSpec{Name: "far", IP: "10.0.0.5"})
	a, b, c := h.model("a"), h.model("b"), h.model("c")

	port := func(d Deployment) int { return h.row(d.ID).hostPort }
	d1 := h.deploy(a, "c1", "n1")
	d2 := h.deploy(a, "c1", "n2")
	d3 := h.deploy(a, "c1", "far")
	if port(d1) != 18000 || port(d2) != 18001 || port(d3) != 18000 {
		t.Errorf("ports %d %d %d, want 18000 18001 18000", port(d1), port(d2), port(d3))
	}
	_, err := h.m.CreateDeployment(t.Context(), b.ID, "c1", "n1")
	assertStatus(t, err, http.StatusServiceUnavailable)

	if err := h.m.DeleteDeployment(t.Context(), d1.ID); err != nil {
		t.Fatal(err)
	}
	// Still reserved until the loop has removed the service.
	_, err = h.m.CreateDeployment(t.Context(), b.ID, "c1", "n1")
	assertStatus(t, err, http.StatusServiceUnavailable)
	h.tick()
	if !h.gone(d1.ID) {
		t.Fatal("deleted row survived")
	}
	if d4 := h.deploy(c, "c1", "n2"); port(d4) != 18000 {
		t.Errorf("freed port not reused: %d", port(d4))
	}
}

func TestProbeFailuresMakeUnhealthy(t *testing.T) {
	h := newHarness(t, nil)
	d := h.deploy(h.model("qwen"), "c1", "n1")
	h.toReady(d.ID)
	en := h.engines.at("127.0.0.1:18000")
	if en == nil {
		t.Fatal("no engine")
	}

	en.healthy.Store(false)
	for range unhealthyAfter - 1 {
		h.tick()
		h.wantState(d.ID, StateReady)
	}
	h.tick()
	if r := h.wantState(d.ID, StateUnhealthy); r.Error == "" {
		t.Error("unhealthy without error")
	}
	h.tick()
	h.wantState(d.ID, StateUnhealthy)

	en.healthy.Store(true)
	h.tick()
	h.wantState(d.ID, StateReady)

	// The failure count starts over after a success.
	en.healthy.Store(false)
	h.tick()
	h.tick()
	en.healthy.Store(true)
	h.tick()
	en.healthy.Store(false)
	h.tick()
	h.tick()
	h.wantState(d.ID, StateReady)
	if n := len(h.fake.Services()); n != 1 {
		t.Errorf("services = %d, want 1: unhealthy must not touch Oakestra", n)
	}
}

func TestScheduleTimeout(t *testing.T) {
	h := newHarness(t, nil)
	d := h.deploy(h.model("qwen"), "c1", "n1")
	// Swallow the deploy request so the service never gets an instance.
	// Fake ids are hex counters, so this prefix only matches service ids.
	h.fake.Fail(oakestratest.Failure{Method: http.MethodPost, Path: "/api/service/0", Status: http.StatusOK})
	h.tick()
	h.wantState(d.ID, StateScheduling)
	h.advance(h.cfg.ScheduleTimeout - time.Second)
	h.tick()
	h.wantState(d.ID, StateScheduling)
	h.advance(2 * time.Second)
	h.tick()
	if r := h.wantState(d.ID, StateFailed); !strings.Contains(r.Error, "within") {
		t.Errorf("error = %q", r.Error)
	}
}

func TestStartupTimeout(t *testing.T) {
	h := newHarness(t, nil)
	d := h.deploy(h.model("qwen"), "c1", "n1")
	h.tick()
	h.tick()
	h.wantState(d.ID, StateStarting)
	h.engines.at("127.0.0.1:18000").healthy.Store(false)
	h.tick()
	if r := h.wantState(d.ID, StateStarting); !strings.Contains(r.Error, "status 503") {
		t.Errorf("probe error not recorded: %q", r.Error)
	}
	h.advance(h.cfg.StartupTimeout + time.Second)
	h.tick()
	if r := h.wantState(d.ID, StateFailed); !strings.Contains(r.Error, "did not become ready") {
		t.Errorf("error = %q", r.Error)
	}
}

func TestStartingFailsFastOnDeadInstance(t *testing.T) {
	h := newHarness(t, nil)
	d := h.deploy(h.model("qwen"), "c1", "n1")
	h.tick()
	h.tick()
	h.wantState(d.ID, StateStarting)
	if err := h.fake.RemoveNode("n1"); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if r := h.wantState(d.ID, StateFailed); !strings.Contains(r.Error, "worker node removed") {
		t.Errorf("error = %q", r.Error)
	}
}

func TestOnChange(t *testing.T) {
	h := newHarness(t, nil)
	var mu sync.Mutex
	var calls [][]Endpoint
	h.m.OnChange(func(eps []Endpoint) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, eps)
	})
	last := func() ([]Endpoint, int) {
		mu.Lock()
		defer mu.Unlock()
		if len(calls) == 0 {
			return nil, 0
		}
		return calls[len(calls)-1], len(calls)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.m.Run(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if eps, n := last(); n == 1 {
			if eps == nil || len(eps) != 0 {
				t.Errorf("startup endpoints = %#v, want empty", eps)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no startup notification")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mod := h.model("qwen")
	d := h.deploy(mod, "c1", "n1")
	h.tick()
	h.tick()
	if _, n := last(); n != 1 {
		t.Errorf("notified %d times before anything became routable", n)
	}
	h.tick()
	eps, n := last()
	if n != 2 || len(eps) != 1 {
		t.Fatalf("after ready: %d calls, endpoints %+v", n, eps)
	}
	r := h.row(d.ID)
	want := Endpoint{DeploymentID: d.ID, ModelID: mod.ID, ModelName: "qwen", Auto: true, Labels: map[string]string{},
		Cluster: "c1", Node: "n1", BaseURL: "http://127.0.0.1:18000", EngineKey: r.engineKey}
	if got := eps[0]; got.DeploymentID != want.DeploymentID || got.ModelID != want.ModelID || got.ModelName != want.ModelName ||
		got.Auto != want.Auto || len(got.Labels) != 0 || got.Cluster != want.Cluster || got.Node != want.Node ||
		got.BaseURL != want.BaseURL || got.EngineKey != want.EngineKey {
		t.Errorf("endpoint = %+v\nwant       %+v", got, want)
	}

	h.tick()
	if _, n := last(); n != 2 {
		t.Errorf("unchanged tick notified (%d calls)", n)
	}

	if _, err := h.m.UpdateModel(t.Context(), mod.ID, ModelPatch{Auto: new(false), Labels: &map[string]string{"tier": "big"}}); err != nil {
		t.Fatal(err)
	}
	eps, n = last()
	if n != 3 || len(eps) != 1 || eps[0].Auto || eps[0].Labels["tier"] != "big" {
		t.Errorf("after model update: %d calls, %+v", n, eps)
	}

	h.m.ReportFailure(d.ID)
	select {
	case <-h.m.kick:
	default:
		t.Error("ReportFailure did not kick the loop")
	}
	h.wantState(d.ID, StateReady)

	if err := h.m.DeleteDeployment(t.Context(), d.ID); err != nil {
		t.Fatal(err)
	}
	eps, n = last()
	if n != 4 || len(eps) != 0 {
		t.Errorf("after delete: %d calls, %+v", n, eps)
	}
}

// Removing a deployment whose service was created without the id reaching
// the row must still remove that service.
func TestDeleteRemovesServiceCreatedWithoutStoredID(t *testing.T) {
	h := newHarness(t, nil)
	mod := h.model("qwen")
	d := h.deploy(mod, "c1", "n1")
	r := h.row(d.ID)

	appID, err := h.m.application(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ms, err := oakestra.BuildMicroservice(h.m.serviceSpec(r, mod))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.oak.CreateService(t.Context(), appID, ms); err != nil {
		t.Fatal(err)
	}

	if err := h.m.DeleteDeployment(t.Context(), d.ID); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if n := len(h.fake.Services()); n != 0 {
		t.Fatalf("services = %d, want 0 (orphan removed)", n)
	}
	if _, err := h.m.GetDeployment(t.Context(), d.ID); err == nil {
		t.Fatal("deployment row still exists")
	}
}
