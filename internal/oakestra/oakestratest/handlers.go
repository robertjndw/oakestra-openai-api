package oakestratest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
)

func (f *Fake) routes() {
	m := http.NewServeMux()
	// System manager.
	m.HandleFunc("POST /api/auth/login", f.login)
	m.HandleFunc("POST /api/auth/refresh", f.refresh)
	m.HandleFunc("GET /api/clusters/{$}", f.listClusters)
	m.HandleFunc("GET /api/clusters", f.listClusters)
	m.HandleFunc("GET /api/user/{name}", f.authed(f.getUser))
	m.HandleFunc("POST /api/application/{$}", f.authed(f.createApplication))
	m.HandleFunc("GET /api/application/{id}", f.authed(f.getApplication))
	m.HandleFunc("DELETE /api/application/{id}", f.authed(f.deleteApplication))
	m.HandleFunc("GET /api/applications/{uid}", f.authed(f.listUserApplications))
	m.HandleFunc("POST /api/service/{$}", f.authed(f.createService))
	m.HandleFunc("GET /api/service/{id}", f.authed(f.getService))
	m.HandleFunc("DELETE /api/service/{id}", f.authed(f.deleteService))
	m.HandleFunc("POST /api/service/{id}/instance", f.authed(f.deployInstance))
	m.HandleFunc("DELETE /api/service/{id}/instance/{n}", f.authed(f.undeployInstance))
	m.HandleFunc("GET /api/services/{appid}", f.authed(f.listAppServices))
	// Resource abstractors: the first cluster at the root, every cluster
	// under its own prefix.
	for _, prefix := range []string{"", "/cluster/{cluster}"} {
		m.HandleFunc("GET "+prefix+"/api/v1/resources/{$}", f.listResources)
		m.HandleFunc("GET "+prefix+"/api/v1/resources", f.listResources)
		m.HandleFunc("GET "+prefix+"/api/v1/resources/{id}", f.getResource)
	}
	f.mux = m
}

type userHandler func(w http.ResponseWriter, r *http.Request, user string)

// authed mimics jwt_auth_required: 401 {"message": ...} without a valid
// access token.
func (f *Fake) authed(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		user, err := f.authLocked(r, false)
		f.mu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"msg": err.Error()})
			return
		}
		h(w, r, user)
	}
}

func message(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"message": fmt.Sprintf(format, args...)})
}

func (f *Fake) login(w http.ResponseWriter, r *http.Request) {
	var req oakestra.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		message(w, http.StatusForbidden, "No credentials provided")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := req.Username != "" && req.Password != ""
	if len(f.opts.Users) > 0 {
		pw, known := f.opts.Users[req.Username]
		ok = known && pw == req.Password
	}
	if !ok {
		message(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	f.logins++
	writeJSON(w, http.StatusOK, oakestra.LoginResponse{
		Token:        f.newTokenLocked(req.Username, false),
		RefreshToken: f.newTokenLocked(req.Username, true),
	})
}

func (f *Fake) refresh(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user, err := f.authLocked(r, true)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"msg": err.Error()})
		return
	}
	f.refreshes++
	writeJSON(w, http.StatusOK, oakestra.LoginResponse{Token: f.newTokenLocked(user, false)})
}

func (f *Fake) getUser(w http.ResponseWriter, r *http.Request, user string) {
	if r.PathValue("name") != user {
		message(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"_id": userID(user), "name": user})
}

// userID derives a stable document id from the user name; the fake has no
// user collection.
func userID(user string) string {
	return fmt.Sprintf("%024x", []byte(user))[:24]
}

func (f *Fake) listClusters(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, f.Clusters())
}

func validSLA(sla *oakestra.SLA, needAppID bool) error {
	if sla.SLAVersion != oakestra.SLAVersion {
		return fmt.Errorf("unsupported sla_version %q", sla.SLAVersion)
	}
	if len(sla.Applications) == 0 {
		return fmt.Errorf("no applications")
	}
	for _, a := range sla.Applications {
		if !oakestra.ValidName(a.ApplicationName) || !oakestra.ValidName(a.ApplicationNamespace) {
			return fmt.Errorf("application %q.%q does not match '^[a-zA-Z0-9]{1,32}$'", a.ApplicationName, a.ApplicationNamespace)
		}
		if a.Microservices == nil {
			return fmt.Errorf("'microservices' is a required property")
		}
		if needAppID && a.ApplicationID == "" {
			return fmt.Errorf("applicationID is required")
		}
		for _, ms := range a.Microservices {
			switch {
			case ms.MicroserviceID != "":
				return fmt.Errorf("microserviceID must be empty")
			case !oakestra.ValidName(ms.MicroserviceName) || !oakestra.ValidName(ms.MicroserviceNamespace):
				return fmt.Errorf("microservice %q.%q does not match '^[a-zA-Z0-9]{1,32}$'", ms.MicroserviceName, ms.MicroserviceNamespace)
			case ms.Virtualization == "" || ms.Code == "":
				return fmt.Errorf("virtualization and code are required")
			}
			for _, c := range ms.Constraints {
				if c.Type == "" {
					return fmt.Errorf("constraint type is required")
				}
			}
			for _, v := range ms.Volumes {
				if v.VolumeID == "" || v.CSIDriver == "" {
					return fmt.Errorf("volume_id and csi_driver are required")
				}
			}
		}
	}
	return nil
}

func (f *Fake) createApplication(w http.ResponseWriter, r *http.Request, user string) {
	var sla oakestra.SLA
	if err := json.NewDecoder(r.Body).Decode(&sla); err != nil {
		message(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	if err := validSLA(&sla, false); err != nil {
		message(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	f.mu.Lock()
	for _, a := range sla.Applications {
		if f.appByNameLocked(user, a.ApplicationName, a.ApplicationNamespace) != nil {
			f.mu.Unlock()
			message(w, http.StatusConflict, "An application with the same name and namespace exists already")
			return
		}
	}
	for _, a := range sla.Applications {
		id := f.newIDLocked()
		f.apps[id] = &app{info: oakestra.ApplicationInfo{
			ApplicationID:        id,
			ApplicationName:      a.ApplicationName,
			ApplicationNamespace: a.ApplicationNamespace,
			ApplicationDesc:      a.ApplicationDesc,
			Microservices:        []string{},
			UserID:               user,
		}}
		f.appOrder = append(f.appOrder, id)
		for _, ms := range a.Microservices {
			f.createServiceLocked(f.apps[id], ms)
		}
	}
	out := f.userAppsLocked(user)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) appByNameLocked(user, name, ns string) *app {
	for _, id := range f.appOrder {
		a := f.apps[id]
		if a.info.UserID == user && a.info.ApplicationName == name && a.info.ApplicationNamespace == ns {
			return a
		}
	}
	return nil
}

func (f *Fake) userAppsLocked(user string) []oakestra.ApplicationInfo {
	out := []oakestra.ApplicationInfo{}
	for _, id := range f.appOrder {
		if a := f.apps[id]; a.info.UserID == user {
			out = append(out, cloneApp(a.info))
		}
	}
	return out
}

func (f *Fake) getApplication(w http.ResponseWriter, r *http.Request, user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.apps[r.PathValue("id")]
	if !ok || a.info.UserID != user {
		message(w, http.StatusNotFound, "Application not found")
		return
	}
	writeJSON(w, http.StatusOK, cloneApp(a.info))
}

func (f *Fake) deleteApplication(w http.ResponseWriter, r *http.Request, user string) {
	f.mu.Lock()
	a, ok := f.apps[r.PathValue("id")]
	if !ok || a.info.UserID != user {
		f.mu.Unlock()
		message(w, http.StatusNotFound, "Application not found")
		return
	}
	var gone []Deployment
	for _, sid := range a.info.Microservices {
		gone = append(gone, f.deleteServiceLocked(sid)...)
	}
	delete(f.apps, a.info.ApplicationID)
	f.appOrder = slices.DeleteFunc(f.appOrder, func(id string) bool { return id == a.info.ApplicationID })
	f.mu.Unlock()
	f.undeployed(gone)
	message(w, http.StatusOK, "Application Deleted")
}

func (f *Fake) listUserApplications(w http.ResponseWriter, r *http.Request, user string) {
	if r.PathValue("uid") != userID(user) {
		message(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	f.mu.Lock()
	out := f.userAppsLocked(user)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) createService(w http.ResponseWriter, r *http.Request, user string) {
	var sla oakestra.SLA
	if err := json.NewDecoder(r.Body).Decode(&sla); err != nil {
		message(w, http.StatusBadRequest, "The given SLA was not formatted correctly")
		return
	}
	// The real endpoint turns every failure inside its try block into a
	// 400, validation errors included.
	if err := validSLA(&sla, true); err != nil {
		message(w, http.StatusBadRequest, "The given SLA was not formatted correctly: %v", err)
		return
	}
	f.mu.Lock()
	a, ok := f.apps[sla.Applications[0].ApplicationID]
	if !ok || a.info.UserID != user {
		f.mu.Unlock()
		message(w, http.StatusBadRequest, "The given SLA was not formatted correctly")
		return
	}
	res := oakestra.CreateServiceResult{DeployedServices: []oakestra.ServiceResult{}, FailedServices: []oakestra.ServiceResult{}}
	for _, ms := range sla.Applications[0].Microservices {
		res.JobID = f.createServiceLocked(a, ms)
		res.DeployedServices = append(res.DeployedServices, oakestra.ServiceResult{ServiceName: ms.MicroserviceName, Status: http.StatusOK})
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, res)
}

// createServiceLocked mirrors generate_db_structure + insert_job.
func (f *Fake) createServiceLocked(a *app, ms oakestra.Microservice) string {
	id := f.newIDLocked()
	virt := ms.Virtualization
	if virt == oakestra.VirtualizationContainer {
		virt = "docker"
	}
	s := &service{view: ServiceView{
		Service: oakestra.Service{
			ID:                    id,
			MicroserviceID:        id,
			ApplicationID:         a.info.ApplicationID,
			AppName:               a.info.ApplicationName,
			AppNS:                 a.info.ApplicationNamespace,
			ServiceName:           ms.MicroserviceName,
			ServiceNS:             ms.MicroserviceNamespace,
			MicroserviceName:      ms.MicroserviceName,
			MicroserviceNamespace: ms.MicroserviceNamespace,
			Image:                 ms.Code,
			Virtualization:        virt,
			Memory:                ms.Memory,
			VGPUs:                 ms.VGPUs,
			Port:                  ms.Port,
			Cmd:                   slices.Clone(ms.Cmd),
			Environment:           slices.Clone(ms.Environment),
			Constraints:           slices.Clone(ms.Constraints),
			InstanceList:          []oakestra.Instance{},
		},
		SLA: ms,
	}}
	f.services[id] = s
	f.svcOrder = append(f.svcOrder, id)
	a.info.Microservices = append(a.info.Microservices, id)
	return id
}

// ownedServiceLocked mirrors get_service: the service must belong to one of
// the user's applications.
func (f *Fake) ownedServiceLocked(user, id string) *service {
	s, ok := f.services[id]
	if !ok {
		return nil
	}
	a, ok := f.apps[s.view.ApplicationID]
	if !ok || a.info.UserID != user {
		return nil
	}
	return s
}

func (f *Fake) getService(w http.ResponseWriter, r *http.Request, user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.ownedServiceLocked(user, r.PathValue("id"))
	if s == nil {
		message(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, cloneView(s.view).Service)
}

func (f *Fake) listAppServices(w http.ResponseWriter, r *http.Request, user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.apps[r.PathValue("appid")]
	if !ok || a.info.UserID != user {
		message(w, http.StatusNotFound, "app not found")
		return
	}
	out := []oakestra.Service{}
	for _, id := range a.info.Microservices {
		if s, ok := f.services[id]; ok {
			out = append(out, cloneView(s.view).Service)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) deleteService(w http.ResponseWriter, r *http.Request, user string) {
	f.mu.Lock()
	if f.ownedServiceLocked(user, r.PathValue("id")) == nil {
		f.mu.Unlock()
		// What the real system manager answers for a missing service.
		message(w, http.StatusInternalServerError, "Job not deleted")
		return
	}
	gone := f.deleteServiceLocked(r.PathValue("id"))
	f.mu.Unlock()
	f.undeployed(gone)
	message(w, http.StatusOK, "Job deleted")
}

func (f *Fake) deleteServiceLocked(id string) []Deployment {
	s, ok := f.services[id]
	if !ok {
		return nil
	}
	var gone []Deployment
	for _, inst := range s.view.InstanceList {
		if inst.Status != oakestra.StatusFailed {
			gone = append(gone, f.deploymentLocked(s, inst, f.nodes[inst.WorkerID]))
		}
	}
	if a, ok := f.apps[s.view.ApplicationID]; ok {
		a.info.Microservices = slices.DeleteFunc(a.info.Microservices, func(x string) bool { return x == id })
	}
	delete(f.services, id)
	f.svcOrder = slices.DeleteFunc(f.svcOrder, func(x string) bool { return x == id })
	return gone
}

func (f *Fake) undeployed(ds []Deployment) {
	if f.opts.OnUndeploy == nil {
		return
	}
	for _, d := range ds {
		f.opts.OnUndeploy(d)
	}
}

// deployInstance places one instance. The real system manager schedules in
// a background thread and answers {"message":"ok"} regardless; the fake
// places synchronously so tests see the result right after the call.
func (f *Fake) deployInstance(w http.ResponseWriter, r *http.Request, user string) {
	f.mu.Lock()
	s := f.ownedServiceLocked(user, r.PathValue("id"))
	if s == nil {
		f.mu.Unlock()
		message(w, http.StatusOK, "ok")
		return
	}
	n, status, detail := f.scheduleLocked(s)
	if n == nil {
		s.view.Status = status
		s.view.StatusDetail = detail
		f.mu.Unlock()
		message(w, http.StatusOK, "ok")
		return
	}
	c := f.clusterByIDLocked(n.clusterID)
	hostPort, _ := firstPortMapping(s.view.Port)
	inst := oakestra.Instance{
		InstanceNumber:  s.view.NextInstanceNumber,
		ClusterID:       c.info.ID,
		ClusterLocation: c.info.Location,
		Status:          oakestra.StatusCreating,
		PublicIP:        n.res.IP,
		HostIP:          n.res.IP,
		WorkerID:        n.res.ID,
		HostPort:        oakestra.FlexInt(hostPort),
		LastModified:    oakestra.Timestamp{Time: f.opts.Now().UTC()},
	}
	s.view.NextInstanceNumber++
	s.view.InstanceList = append(s.view.InstanceList, inst)
	s.view.Status = oakestra.StatusCreating
	d := f.deploymentLocked(s, inst, n)
	f.mu.Unlock()

	var deployErr error
	if f.opts.OnDeploy != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		deployErr = f.opts.OnDeploy(ctx, d)
		cancel()
	}

	f.mu.Lock()
	if s, ok := f.services[d.ServiceID]; ok {
		for i := range s.view.InstanceList {
			inst := &s.view.InstanceList[i]
			if inst.InstanceNumber != d.InstanceNumber {
				continue
			}
			inst.Status, inst.StatusDetail = oakestra.StatusRunning, ""
			if deployErr != nil {
				inst.Status, inst.StatusDetail = oakestra.StatusFailed, deployErr.Error()
			}
			inst.LastModified = oakestra.Timestamp{Time: f.opts.Now().UTC()}
			s.view.Status = inst.Status
			s.view.StatusDetail = inst.StatusDetail
		}
	}
	f.mu.Unlock()
	message(w, http.StatusOK, "ok")
}

// scheduleLocked honours a direct constraint by candidate names, like the
// cpumemfit scheduler at root (cluster) and cluster (node) level, and like
// it refuses a node that lacks a CSI driver the service's volumes need.
// Without a direct constraint it takes the first active node that has the
// drivers.
func (f *Fake) scheduleLocked(s *service) (n *node, status, detail string) {
	for _, c := range s.view.Constraints {
		if c.Type != oakestra.ConstraintDirect {
			continue
		}
		cl := f.clusterByNameLocked(c.Cluster)
		if cl == nil {
			return nil, oakestra.StatusTargetClusterNotFound, "cluster " + c.Cluster + " not found"
		}
		for _, id := range f.nodeOrder {
			cand := f.nodes[id]
			if cand.clusterID != cl.info.ID || cand.res.CandidateName != c.Node {
				continue
			}
			if !cand.active {
				return nil, oakestra.StatusNoQualifiedWorkerFound, "node " + c.Node + " is not active"
			}
			// The real scheduler filters the pinned node out and reports the
			// generic no-capacity status with no detail; the detail here is
			// what a human would find in its debug log.
			if missing := missingCSIDriver(s.view.SLA.Volumes, cand); missing != "" {
				return nil, oakestra.StatusNoActiveClusterWithCap, fmt.Sprintf(
					"node %s does not have required CSI driver %s (available: %v)", c.Node, missing, driverNames(cand))
			}
			return cand, "", ""
		}
		return nil, oakestra.StatusNoQualifiedWorkerFound, "node " + c.Node + " not found in " + c.Cluster
	}
	for _, id := range f.nodeOrder {
		if cand := f.nodes[id]; cand.active && missingCSIDriver(s.view.SLA.Volumes, cand) == "" {
			return cand, "", ""
		}
	}
	return nil, oakestra.StatusNoActiveClusterWithCap, "no active node with the required CSI drivers"
}

// missingCSIDriver mirrors hasRequiredCSIDrivers of the cpumemfit
// scheduler: it returns the first driver a volume names that the node does
// not advertise, or "". Volumes without a driver do not constrain
// placement there either.
func missingCSIDriver(vols []oakestra.Volume, n *node) string {
	have := driverNames(n)
	for _, v := range vols {
		if v.CSIDriver != "" && !slices.Contains(have, v.CSIDriver) {
			return v.CSIDriver
		}
	}
	return ""
}

func driverNames(n *node) []string {
	names := make([]string, 0, len(n.res.CSIDrivers))
	for _, d := range n.res.CSIDrivers {
		names = append(names, d.Name)
	}
	return names
}

func (f *Fake) undeployInstance(w http.ResponseWriter, r *http.Request, user string) {
	num, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		message(w, http.StatusBadRequest, "instance number %q is not an integer", r.PathValue("n"))
		return
	}
	f.mu.Lock()
	s := f.ownedServiceLocked(user, r.PathValue("id"))
	var gone []Deployment
	if s != nil {
		s.view.InstanceList = slices.DeleteFunc(s.view.InstanceList, func(inst oakestra.Instance) bool {
			if inst.InstanceNumber != num {
				return false
			}
			if inst.Status != oakestra.StatusFailed {
				gone = append(gone, f.deploymentLocked(s, inst, f.nodes[inst.WorkerID]))
			}
			return true
		})
	}
	f.mu.Unlock()
	f.undeployed(gone)
	message(w, http.StatusOK, "ok")
}

// abstractorCluster resolves the cluster an abstractor request is for: the
// {cluster} path value, or the first cluster for root paths.
func (f *Fake) abstractorClusterLocked(r *http.Request) *cluster {
	if name := r.PathValue("cluster"); name != "" {
		return f.clusterByNameLocked(name)
	}
	if len(f.clusters) == 0 {
		return nil
	}
	return f.clusters[0]
}

func (f *Fake) listResources(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.abstractorClusterLocked(r)
	if c == nil {
		message(w, http.StatusNotFound, "cluster not found")
		return
	}
	activeOnly := r.URL.Query().Get("active") == "true"
	out := []oakestra.NodeResource{}
	for _, id := range f.nodeOrder {
		n := f.nodes[id]
		if n.clusterID != c.info.ID || (activeOnly && !n.active) {
			continue
		}
		out = append(out, f.nodeViewLocked(n))
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) getResource(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.abstractorClusterLocked(r)
	n, ok := f.nodes[r.PathValue("id")]
	if c == nil || !ok || n.clusterID != c.info.ID {
		message(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, f.nodeViewLocked(n))
}
