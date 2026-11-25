package oakestra_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra/oakestratest"
)

// clock is a manual clock shared by the fake and the client, so token
// expiry is tested without sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	fake   *oakestratest.Fake
	srv    *httptest.Server
	client *oakestra.HTTPClient
	clock  *clock
}

func newEnv(t *testing.T, fopts oakestratest.Options) *env {
	t.Helper()
	clk := newClock()
	fopts.Now = clk.Now
	fake := oakestratest.New(fopts)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	opts := oakestratest.ClientOptions(srv.URL, "ooa", "secret")
	opts.Now = clk.Now
	c, err := oakestra.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return &env{fake: fake, srv: srv, client: c, clock: clk}
}

func (e *env) seed(t *testing.T) {
	t.Helper()
	e.fake.AddCluster("edge", "10.0.0.2")
	if _, err := e.fake.AddNode("edge", oakestratest.NodeSpec{Name: "dgx1", IP: "10.0.0.11"}); err != nil {
		t.Fatal(err)
	}
}

func engineMicroservice(t *testing.T, name string) oakestra.Microservice {
	t.Helper()
	ms, err := oakestra.BuildMicroservice(oakestra.ServiceSpec{
		Name: name, Namespace: "ooa", Image: "fake:latest",
		HostPort: 18000, ContainerPort: 8000, MemoryMB: 1024, VGPUs: 1,
		Cluster: "edge", Node: "dgx1",
		Volumes: []oakestra.Volume{{
			VolumeID: "d_1", CSIDriver: oakestra.ModelpackCSIDriver, MountPath: "/model",
			Config: map[string]string{oakestra.ModelpackReferenceKey: "registry.example/m:1"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestServiceLifecycle(t *testing.T) {
	var deployed, undeployed []oakestratest.Deployment
	var mu sync.Mutex
	e := newEnv(t, oakestratest.Options{
		OnDeploy: func(_ context.Context, d oakestratest.Deployment) error {
			mu.Lock()
			defer mu.Unlock()
			deployed = append(deployed, d)
			return nil
		},
		OnUndeploy: func(d oakestratest.Deployment) {
			mu.Lock()
			defer mu.Unlock()
			undeployed = append(undeployed, d)
		},
	})
	e.seed(t)
	ctx := t.Context()

	app, err := oakestra.NewApplication("openaiapi", "ooa", "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := e.client.CreateApplication(ctx, app)
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if info.ApplicationID == "" || info.ApplicationName != "openaiapi" {
		t.Fatalf("app = %+v", info)
	}
	if _, err := e.client.CreateApplication(ctx, app); !errors.Is(err, oakestra.ErrConflict) {
		t.Fatalf("second CreateApplication: %v, want ErrConflict", err)
	}
	apps, err := e.client.GetApplications(ctx)
	if err != nil || len(apps) != 1 || apps[0].ApplicationID != info.ApplicationID {
		t.Fatalf("GetApplications = %+v, %v", apps, err)
	}

	sid, err := e.client.CreateService(ctx, info.ApplicationID, engineMicroservice(t, "qwen123456"))
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if err := e.client.DeployInstance(ctx, sid); err != nil {
		t.Fatalf("DeployInstance: %v", err)
	}
	svc, err := e.client.GetService(ctx, sid)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if svc.Virtualization != "docker" || svc.Port != "18000:8000" || len(svc.InstanceList) != 1 {
		t.Fatalf("service = %+v", svc)
	}
	inst := svc.InstanceList[0]
	if inst.Status != oakestra.StatusRunning || inst.PublicIP != "10.0.0.11" || inst.HostPort != 18000 {
		t.Fatalf("instance = %+v", inst)
	}
	if len(deployed) != 1 || deployed[0].HostPort != 18000 || deployed[0].Node.CandidateName != "dgx1" || deployed[0].ClusterName != "edge" {
		t.Fatalf("OnDeploy saw %+v", deployed)
	}

	if len(deployed[0].Volumes) != 1 || deployed[0].Volumes[0].Config[oakestra.ModelpackReferenceKey] != "registry.example/m:1" {
		t.Fatalf("OnDeploy volumes = %+v", deployed[0].Volumes)
	}
	listed, err := e.client.ListServices(ctx, info.ApplicationID)
	if err != nil || len(listed) != 1 || listed[0].ID != sid || listed[0].MicroserviceName != "qwen123456" {
		t.Fatalf("ListServices = %+v, %v", listed, err)
	}

	if err := e.client.UndeployInstance(ctx, sid, inst.InstanceNumber); err != nil {
		t.Fatalf("UndeployInstance: %v", err)
	}
	if len(undeployed) != 1 {
		t.Fatalf("OnUndeploy calls = %d", len(undeployed))
	}
	if err := e.client.DeleteService(ctx, sid); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if err := e.client.DeleteService(ctx, sid); !errors.Is(err, oakestra.ErrNotFound) {
		t.Fatalf("DeleteService(missing) = %v, want ErrNotFound", err)
	}
	if _, err := e.client.GetService(ctx, sid); !errors.Is(err, oakestra.ErrNotFound) {
		t.Fatalf("GetService(missing) = %v, want ErrNotFound", err)
	}
}

func TestDeployOnMissingNodeFailsScheduling(t *testing.T) {
	e := newEnv(t, oakestratest.Options{})
	e.seed(t)
	ctx := t.Context()
	app, _ := oakestra.NewApplication("openaiapi", "ooa", "")
	info, err := e.client.CreateApplication(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	ms := engineMicroservice(t, "qwen1")
	ms.Constraints[0].Node = "dgx9"
	sid, err := e.client.CreateService(ctx, info.ApplicationID, ms)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.client.DeployInstance(ctx, sid); err != nil {
		t.Fatal(err)
	}
	svc, err := e.client.GetService(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(svc.InstanceList) != 0 || svc.Status != oakestra.StatusNoQualifiedWorkerFound {
		t.Fatalf("service = %+v", svc)
	}
}

func TestTokenRefreshBeforeExpiry(t *testing.T) {
	e := newEnv(t, oakestratest.Options{})
	e.seed(t)
	ctx := t.Context()
	if _, err := e.client.GetApplications(ctx); err != nil {
		t.Fatal(err)
	}
	if e.fake.Logins() != 1 || e.fake.Refreshes() != 0 {
		t.Fatalf("logins %d refreshes %d", e.fake.Logins(), e.fake.Refreshes())
	}

	// Inside the refresh margin, but not yet expired: refreshed proactively.
	e.clock.Advance(9*time.Minute + 30*time.Second)
	if _, err := e.client.GetApplications(ctx); err != nil {
		t.Fatal(err)
	}
	if e.fake.Logins() != 1 || e.fake.Refreshes() != 1 {
		t.Fatalf("after 9.5 min: logins %d refreshes %d", e.fake.Logins(), e.fake.Refreshes())
	}

	// Refresh token expired as well: full login again.
	e.clock.Advance(8 * 24 * time.Hour)
	if _, err := e.client.GetApplications(ctx); err != nil {
		t.Fatal(err)
	}
	if e.fake.Logins() != 2 {
		t.Fatalf("after 8 days: logins %d", e.fake.Logins())
	}
}

func TestRetryOnceOn401(t *testing.T) {
	e := newEnv(t, oakestratest.Options{})
	e.seed(t)
	ctx := t.Context()
	if _, err := e.client.GetApplications(ctx); err != nil {
		t.Fatal(err)
	}
	e.fake.ExpireTokens()
	if _, err := e.client.GetApplications(ctx); err != nil {
		t.Fatalf("after server-side expiry: %v", err)
	}
	if e.fake.Refreshes() != 1 {
		t.Fatalf("refreshes = %d, want 1", e.fake.Refreshes())
	}

	// A persistent 401 is reported, not retried forever.
	e.fake.Fail(oakestratest.Failure{Path: "/api/applications/", Status: http.StatusUnauthorized, Message: "nope"})
	_, err := e.client.GetApplications(ctx)
	if !errors.Is(err, oakestra.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestWrongCredentials(t *testing.T) {
	e := newEnv(t, oakestratest.Options{Users: map[string]string{"ooa": "other"}})
	_, err := e.client.GetApplications(t.Context())
	if !errors.Is(err, oakestra.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestConcurrentCallsLogInOnce(t *testing.T) {
	e := newEnv(t, oakestratest.Options{})
	e.seed(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			if _, err := e.client.GetApplications(ctx); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := e.fake.Logins(); n != 1 {
		t.Fatalf("logins = %d, want 1", n)
	}

	// Near expiry, concurrent callers share one refresh as well.
	e.clock.Advance(9*time.Minute + 30*time.Second)
	for range 20 {
		wg.Go(func() {
			if _, err := e.client.GetApplications(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := e.fake.Refreshes(); n != 1 {
		t.Fatalf("refreshes = %d, want 1", n)
	}
}

func TestErrorMapping(t *testing.T) {
	e := newEnv(t, oakestratest.Options{})
	e.seed(t)
	ctx := t.Context()
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusServiceUnavailable, oakestra.ErrUnavailable},
		{http.StatusForbidden, oakestra.ErrForbidden},
		{http.StatusUnprocessableEntity, oakestra.ErrInvalid},
	}
	for _, tt := range tests {
		e.fake.Fail(oakestratest.Failure{Path: "/api/service/", Status: tt.status, Message: "injected", Times: 1})
		err := e.client.DeployInstance(ctx, "x")
		if !errors.Is(err, tt.want) {
			t.Errorf("status %d: err = %v, want %v", tt.status, err, tt.want)
		}
		var apiErr *oakestra.APIError
		if !errors.As(err, &apiErr) || apiErr.Message != "injected" {
			t.Errorf("status %d: APIError = %+v", tt.status, apiErr)
		}
	}
}

func TestNetworkErrorIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c, err := oakestra.New(oakestra.Options{API: srv.URL, User: "ooa", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListClusters(t.Context()); !errors.Is(err, oakestra.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestRequestTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(block)
		srv.Close()
	})
	c, err := oakestra.New(oakestra.Options{API: srv.URL, User: "ooa", Password: "x", Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListClusters(t.Context()); !errors.Is(err, oakestra.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestClustersAndNodes(t *testing.T) {
	e := newEnv(t, oakestratest.Options{})
	e.fake.AddCluster("edge", "10.0.0.2")
	e.fake.AddCluster("lab", "10.0.1.2")
	for _, n := range []struct{ cluster, name string }{{"edge", "dgx1"}, {"edge", "dgx2"}, {"lab", "jetson"}} {
		if _, err := e.fake.AddNode(n.cluster, oakestratest.NodeSpec{Name: n.name, CSIDrivers: csiFor(n.name)}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := t.Context()
	clusters, err := e.client.ListClusters(ctx)
	if err != nil || len(clusters) != 2 {
		t.Fatalf("ListClusters = %+v, %v", clusters, err)
	}
	if clusters[0].DisplayName() != "edge" || clusters[0].IP != "10.0.0.2" || clusters[0].ActiveNodes != 2 {
		t.Fatalf("cluster = %+v", clusters[0])
	}
	nodes, err := e.client.ListNodes(ctx, clusters[1])
	if err != nil || len(nodes) != 1 {
		t.Fatalf("ListNodes(lab) = %+v, %v", nodes, err)
	}
	n := nodes[0]
	if n.CandidateName != "jetson" || !n.Active || n.Architecture != "arm64" || len(n.CSIDrivers) != 0 {
		t.Fatalf("node = %+v", n)
	}
	if n.MemoryMB != 131072-4096 || n.MemoryPercent != 3.1 {
		t.Fatalf("memory = %d MB, %.1f%%", n.MemoryMB, n.MemoryPercent)
	}
}

func TestListNodesRequestsExtraFields(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("resources")
		_, _ = io.WriteString(w, "[]")
	}))
	t.Cleanup(srv.Close)
	c, err := oakestra.New(oakestra.Options{API: srv.URL, User: "ooa", AbstractorURL: func(_, _, _ string) string { return srv.URL }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListNodes(t.Context(), oakestra.Cluster{Name: "edge"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"active", "architecture", "last_modified_timestamp"} {
		if !strings.Contains(got, f) {
			t.Errorf("resources=%q lacks %s", got, f)
		}
	}
}

func TestLoginSendsOrganization(t *testing.T) {
	var login oakestra.LoginRequest
	var userPath, appsPath string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&login)
		_, _ = io.WriteString(w, `{"token":"t1","refresh_token":"r1"}`)
	})
	mux.HandleFunc("GET /api/user/{name}", func(w http.ResponseWriter, r *http.Request) {
		userPath = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer t1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Extended JSON id, as older pymongo encoders produce.
		_, _ = io.WriteString(w, `{"_id":{"$oid":"64aa00000000000000000001"},"name":"ooa"}`)
	})
	mux.HandleFunc("GET /api/applications/{uid}", func(w http.ResponseWriter, r *http.Request) {
		appsPath = r.URL.Path
		_, _ = io.WriteString(w, `[{"applicationID":"a1","application_name":"openaiapi","application_namespace":"ooa","microservices":["s1"],"userId":"ooa"}]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := oakestra.New(oakestra.Options{API: srv.URL, User: "ooa", Password: "pw", Org: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	apps, err := c.GetApplications(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if login.Username != "ooa" || login.Password != "pw" || login.OrganizationName != "lab" {
		t.Errorf("login body = %+v", login)
	}
	if userPath != "/api/user/ooa" || appsPath != "/api/applications/64aa00000000000000000001" {
		t.Errorf("paths %q %q", userPath, appsPath)
	}
	if len(apps) != 1 || apps[0].Microservices[0] != "s1" {
		t.Errorf("apps = %+v", apps)
	}
}

func TestCreateServiceReportsFailedServices(t *testing.T) {
	var sla oakestra.SLA
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"token":"t1"}`)
	})
	mux.HandleFunc("GET /api/user/{name}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"_id":"u1"}`)
	})
	mux.HandleFunc("GET /api/applications/{uid}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"applicationID":"a1","application_name":"openaiapi","application_namespace":"ooa","microservices":[]}]`)
	})
	mux.HandleFunc("POST /api/service/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sla)
		_, _ = io.WriteString(w, `{"job_id":"","deployed_services":[],"failed_services":[{"service_name":"e1","status":500,"message":"failed to deploy service: net down"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := oakestra.New(oakestra.Options{API: srv.URL, User: "ooa", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateService(t.Context(), "a1", engineMicroservice(t, "e1"))
	var apiErr *oakestra.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 500 || !strings.Contains(apiErr.Message, "net down") {
		t.Fatalf("err = %v", err)
	}
	app := sla.Applications[0]
	if app.ApplicationID != "a1" || app.ApplicationName != "openaiapi" || app.ApplicationNamespace != "ooa" || sla.SLAVersion != "v2.0" {
		t.Fatalf("sent SLA = %+v", sla)
	}
}

// csiFor gives the "jetson" test node no CSI drivers and every other node
// the defaults.
func csiFor(name string) []string {
	if name == "jetson" {
		return []string{}
	}
	return nil
}

func TestEnsureApplication(t *testing.T) {
	e := newEnv(t, oakestratest.Options{})
	ctx := t.Context()
	app, err := oakestra.NewApplication("openaiapi", "ooa", "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := oakestra.EnsureApplication(ctx, e.client, app)
	if err != nil || first.ApplicationID == "" {
		t.Fatalf("first EnsureApplication = %+v, %v", first, err)
	}
	again, err := oakestra.EnsureApplication(ctx, e.client, app)
	if err != nil || again.ApplicationID != first.ApplicationID {
		t.Fatalf("second EnsureApplication = %+v, %v", again, err)
	}
	if n := len(e.fake.Applications()); n != 1 {
		t.Fatalf("applications = %d", n)
	}

	// Someone else owns the name: the conflict is reported, not hidden.
	other, err := oakestra.New(oakestratest.ClientOptions(e.srv.URL, "someone", "pw"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.CreateApplication(ctx, mustApp(t, "taken", "ooa")); err != nil {
		t.Fatal(err)
	}
	e.fake.Fail(oakestratest.Failure{Method: http.MethodPost, Path: "/api/application/", Status: http.StatusConflict, Message: "exists", Times: 1})
	if _, err := oakestra.EnsureApplication(ctx, e.client, mustApp(t, "taken", "ooa")); !errors.Is(err, oakestra.ErrConflict) {
		t.Fatalf("EnsureApplication(foreign) = %v, want ErrConflict", err)
	}
}

func mustApp(t *testing.T, name, ns string) oakestra.Application {
	t.Helper()
	app, err := oakestra.NewApplication(name, ns, "")
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// A model volume on a node without the modelpack driver never reaches the
// node: the scheduler refuses it and the service says why.
func TestDeployWithoutCSIDriverFailsScheduling(t *testing.T) {
	var deployed atomic.Int32
	e := newEnv(t, oakestratest.Options{
		OnDeploy: func(context.Context, oakestratest.Deployment) error { deployed.Add(1); return nil },
	})
	e.fake.AddCluster("edge", "10.0.0.2")
	if _, err := e.fake.AddNode("edge", oakestratest.NodeSpec{Name: "dgx1", CSIDrivers: []string{oakestra.HostpathCSIDriver}}); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	info, err := oakestra.EnsureApplication(ctx, e.client, mustApp(t, "openaiapi", "ooa"))
	if err != nil {
		t.Fatal(err)
	}
	sid, err := e.client.CreateService(ctx, info.ApplicationID, engineMicroservice(t, "qwen1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.client.DeployInstance(ctx, sid); err != nil {
		t.Fatal(err)
	}
	svc, err := e.client.GetService(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(svc.InstanceList) != 0 || svc.Status != oakestra.StatusNoActiveClusterWithCap || deployed.Load() != 0 {
		t.Fatalf("service = %+v, OnDeploy calls %d", svc, deployed.Load())
	}
	if !strings.Contains(svc.StatusDetail, oakestra.ModelpackCSIDriver) || !strings.Contains(svc.StatusDetail, "dgx1") {
		t.Fatalf("status_detail %q does not name the node and the missing driver", svc.StatusDetail)
	}

	// Installing the driver on the node makes the next attempt succeed.
	drivers := []string{oakestra.HostpathCSIDriver, oakestra.ModelpackCSIDriver}
	if _, err := e.fake.UpdateNode("dgx1", oakestratest.NodeUpdate{CSIDrivers: &drivers}); err != nil {
		t.Fatal(err)
	}
	if err := e.client.DeployInstance(ctx, sid); err != nil {
		t.Fatal(err)
	}
	svc, err = e.client.GetService(ctx, sid)
	if err != nil || len(svc.InstanceList) != 1 || svc.Status != oakestra.StatusRunning || deployed.Load() != 1 {
		t.Fatalf("after installing the driver: %+v, %v, OnDeploy calls %d", svc, err, deployed.Load())
	}
}
