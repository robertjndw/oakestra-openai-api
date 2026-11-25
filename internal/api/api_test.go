package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/robertjndw/oakestra-openai-api/internal/auth"
	"github.com/robertjndw/oakestra-openai-api/internal/config"
	"github.com/robertjndw/oakestra-openai-api/internal/deploy"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra/oakestratest"
	"github.com/robertjndw/oakestra-openai-api/internal/scheduler"
	"github.com/robertjndw/oakestra-openai-api/internal/store"
)

type recordingGateway struct {
	mu  sync.Mutex
	got []string
}

func (g *recordingGateway) SetStrategy(s scheduler.Strategy) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.got = append(g.got, s.Name())
}

func (g *recordingGateway) strategies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.got...)
}

type env struct {
	t       *testing.T
	db      *store.DB
	gw      *recordingGateway
	handler http.Handler
	token   string
}

// testStrategy is registered once for the whole test binary, so settings
// tests have a second strategy to switch to.
const testStrategy = "api-test-first"

func init() {
	scheduler.Register(testStrategy, func(cfg json.RawMessage, _ scheduler.Deps) (scheduler.Strategy, error) {
		return firstStrategy{}, nil
	})
}

type firstStrategy struct{}

func (firstStrategy) Name() string { return testStrategy }
func (firstStrategy) Rank(_ context.Context, _ scheduler.Request, c []scheduler.Candidate) ([]scheduler.Candidate, error) {
	return c, nil
}

// newEnv serves the API in none mode (the dev admin) unless user is set,
// in which case a fake system manager vouches for user and its roles.
func newEnv(t *testing.T, user *auth.User) *env {
	t.Helper()
	cfg, err := config.LoadFrom(map[string]string{
		"OOA_OAKESTRA_USER": "svc", "OOA_OAKESTRA_PASSWORD": "pw", "OOA_ADMIN_AUTH": "none",
	})
	if err != nil {
		t.Fatal(err)
	}

	fake := oakestratest.New(oakestratest.Options{})
	fake.AddCluster("c1", "10.0.0.1")
	for _, spec := range []oakestratest.NodeSpec{
		{Name: "good"},
		{Name: "plain", CSIDrivers: []string{oakestra.HostpathCSIDriver}},
	} {
		if _, err := fake.AddNode("c1", spec); err != nil {
			t.Fatal(err)
		}
	}
	oakSrv := httptest.NewServer(fake)
	t.Cleanup(oakSrv.Close)
	oak, err := oakestra.New(oakestratest.ClientOptions(oakSrv.URL, "svc", "pw"))
	if err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m, err := deploy.New(deploy.Options{Config: cfg, DB: db, Oakestra: oak, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}

	e := &env{t: t, db: db, gw: &recordingGateway{}}
	opts := Options{
		DB: db, Deploy: m, Keys: auth.NewKeys(db), Gateway: e.gw,
		Scheduler: SchedulerSetting{Strategy: scheduler.LeastInflight, Config: json.RawMessage("null")},
		AuthMode:  config.AdminAuthNone,
		PublicURL: "http://gw.example", Version: "test",
		Logger: slog.New(slog.DiscardHandler),
	}
	if user != nil {
		sm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/user/"+user.Name {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": user.Name, "roles": user.Roles})
		}))
		t.Cleanup(sm.Close)
		opts.AuthMode = config.AdminAuthIntrospect
		opts.Introspector = auth.NewIntrospector(sm.URL+"/api", nil)
		e.token = jwt(user.Name)
	}
	e.handler = New(opts).Handler()
	return e
}

func jwt(sub string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"sub":"`+sub+`"}`)) + ".sig"
}

func (e *env) do(method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, Prefix+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// want checks the status and decodes the body into a generic value.
func (e *env) want(rec *httptest.ResponseRecorder, status int) map[string]any {
	e.t.Helper()
	if rec.Code != status {
		e.t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			e.t.Fatalf("body %q: %v", rec.Body.String(), err)
		}
	}
	return out
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func TestEmptyListsAreArrays(t *testing.T) {
	e := newEnv(t, nil)
	for path, field := range map[string]string{"/models": "models", "/deployments": "deployments", "/keys": "keys"} {
		rec := e.do(http.MethodGet, path, "")
		if want := `{"` + field + `":[]}`; strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("GET %s = %s, want %s", path, rec.Body.String(), want)
		}
	}
}

func TestMeAndOverview(t *testing.T) {
	e := newEnv(t, nil)
	me := e.want(e.do(http.MethodGet, "/me", ""), http.StatusOK)
	if me["name"] != "dev" || me["admin"] != true {
		t.Fatalf("me = %v", me)
	}

	e.want(e.do(http.MethodPost, "/models", `{"name":"m1","reference":"r/m:1","memory_mb":1024}`), http.StatusCreated)
	e.want(e.do(http.MethodPost, "/keys", `{"name":"k"}`), http.StatusCreated)
	ov := e.want(e.do(http.MethodGet, "/overview", ""), http.StatusOK)
	oak, _ := ov["oakestra"].(map[string]any)
	deps, _ := ov["deployments"].(map[string]any)
	if ov["public_url"] != "http://gw.example" || ov["version"] != "test" || ov["models"] != 1.0 ||
		ov["keys"] != 1.0 || ov["scheduler"] != scheduler.LeastInflight || oak["reachable"] != true ||
		deps["total"] != 0.0 || deps["ready"] != 0.0 {
		t.Fatalf("overview = %v", ov)
	}
}

func TestModelsCRUD(t *testing.T) {
	e := newEnv(t, nil)
	m := e.want(e.do(http.MethodPost, "/models", `{"name":"m1","reference":"r/m:1","memory_mb":1024}`), http.StatusCreated)
	id, _ := m["id"].(string)
	if id == "" || m["auto"] != true || m["command"] != nil {
		t.Fatalf("created = %v", m)
	}
	if got := e.want(e.do(http.MethodGet, "/models/"+id, ""), http.StatusOK); got["name"] != "m1" {
		t.Fatalf("get = %v", got)
	}
	if got := e.want(e.do(http.MethodPatch, "/models/"+id, `{"auto":false}`), http.StatusOK); got["auto"] != false {
		t.Fatalf("patched = %v", got)
	}
	list := e.want(e.do(http.MethodGet, "/models", ""), http.StatusOK)
	if models, _ := list["models"].([]any); len(models) != 1 {
		t.Fatalf("list = %v", list)
	}
	if body := e.want(e.do(http.MethodPost, "/models", `{"name":"auto","reference":"r","memory_mb":1}`), http.StatusUnprocessableEntity); errCode(body) != "unprocessable" {
		t.Fatalf("auto name accepted: %v", body)
	}
	e.want(e.do(http.MethodDelete, "/models/"+id, ""), http.StatusNoContent)
	if body := e.want(e.do(http.MethodGet, "/models/"+id, ""), http.StatusNotFound); errCode(body) != "not_found" {
		t.Fatalf("after delete: %v", body)
	}
}

func TestNodesAndDeployments(t *testing.T) {
	e := newEnv(t, nil)
	nodes := e.want(e.do(http.MethodGet, "/nodes", ""), http.StatusOK)
	if list, _ := nodes["nodes"].([]any); len(list) != 2 {
		t.Fatalf("nodes = %v", nodes)
	}
	if w, _ := nodes["warnings"].([]any); w == nil || len(w) != 0 {
		t.Fatalf("warnings = %v", nodes["warnings"])
	}

	m := e.want(e.do(http.MethodPost, "/models", `{"name":"m1","reference":"r/m:1","memory_mb":1024}`), http.StatusCreated)
	model, _ := m["id"].(string)

	e.want(e.do(http.MethodPost, "/deployments", `{"model_id":"`+model+`","cluster":"c1","node":"plain"}`), http.StatusUnprocessableEntity)
	e.want(e.do(http.MethodPost, "/deployments", `{"model_id":"`+model+`"}`), http.StatusUnprocessableEntity)
	d := e.want(e.do(http.MethodPost, "/deployments", `{"model_id":"`+model+`","cluster":"c1","node":"good"}`), http.StatusCreated)
	id, _ := d["id"].(string)
	if d["state"] != "pending" || d["model_name"] != "m1" {
		t.Fatalf("deployment = %v", d)
	}
	list := e.want(e.do(http.MethodGet, "/deployments", ""), http.StatusOK)
	if ds, _ := list["deployments"].([]any); len(ds) != 1 {
		t.Fatalf("list = %v", list)
	}

	// Only a failed deployment can be retried.
	if body := e.want(e.do(http.MethodPost, "/deployments/"+id+"/retry", ""), http.StatusConflict); errCode(body) != "invalid_state" {
		t.Fatalf("retry = %v", body)
	}
	e.want(e.do(http.MethodDelete, "/deployments/"+id, ""), http.StatusAccepted)
	got := e.want(e.do(http.MethodGet, "/deployments", ""), http.StatusOK)
	if ds, _ := got["deployments"].([]any); len(ds) != 1 || obj(ds[0])["state"] != "deleting" {
		t.Fatalf("after delete = %v", got)
	}
	e.want(e.do(http.MethodDelete, "/deployments/d_missing", ""), http.StatusNotFound)
}

func TestKeys(t *testing.T) {
	e := newEnv(t, nil)
	created := e.want(e.do(http.MethodPost, "/keys", `{"name":"ci"}`), http.StatusCreated)
	secret, _ := created["secret"].(string)
	key, _ := created["key"].(map[string]any)
	id, _ := key["id"].(string)
	if !strings.HasPrefix(secret, auth.KeyPrefix) || id == "" {
		t.Fatalf("created = %v", created)
	}
	if list := e.want(e.do(http.MethodGet, "/keys", ""), http.StatusOK); len(arr(list["keys"])) != 1 {
		t.Fatalf("list = %v", list)
	}
	e.want(e.do(http.MethodDelete, "/keys/"+id, ""), http.StatusNoContent)
	if list := e.want(e.do(http.MethodGet, "/keys", ""), http.StatusOK); len(arr(list["keys"])) != 0 {
		t.Fatalf("revoked key still listed: %v", list)
	}
	e.want(e.do(http.MethodPost, "/keys", `{"name":""}`), http.StatusUnprocessableEntity)
}

func TestSettingsSwap(t *testing.T) {
	e := newEnv(t, nil)
	got := e.want(e.do(http.MethodGet, "/settings", ""), http.StatusOK)
	sched, _ := got["scheduler"].(map[string]any)
	if sched["strategy"] != scheduler.LeastInflight {
		t.Fatalf("settings = %v", got)
	}

	got = e.want(e.do(http.MethodPut, "/settings", `{"scheduler":{"strategy":"`+testStrategy+`","config":{"a":1}}}`), http.StatusOK)
	if s := obj(got["scheduler"]); s["strategy"] != testStrategy {
		t.Fatalf("put = %v", got)
	}
	if gw := e.gw.strategies(); len(gw) != 1 || gw[0] != testStrategy {
		t.Fatalf("gateway strategies = %v", gw)
	}
	saved, ok, err := store.GetSettingAs[SchedulerSetting](t.Context(), e.db, store.SettingScheduler)
	if err != nil || !ok || saved.Strategy != testStrategy || string(saved.Config) != `{"a":1}` {
		t.Fatalf("persisted = %+v %v %v", saved, ok, err)
	}

	body := e.want(e.do(http.MethodPut, "/settings", `{"scheduler":{"strategy":"nope","config":null}}`), http.StatusUnprocessableEntity)
	if errCode(body) != "unprocessable" {
		t.Fatalf("unknown strategy = %v", body)
	}
	if gw := e.gw.strategies(); len(gw) != 1 {
		t.Fatalf("a rejected strategy reached the gateway: %v", gw)
	}
	if s := obj(e.want(e.do(http.MethodGet, "/settings", ""), http.StatusOK)["scheduler"]); s["strategy"] != testStrategy {
		t.Fatalf("current strategy changed by a rejected PUT: %v", s)
	}
	if ov := e.want(e.do(http.MethodGet, "/overview", ""), http.StatusOK); ov["scheduler"] != testStrategy {
		t.Fatalf("overview scheduler = %v", ov["scheduler"])
	}
}

func TestLoadScheduler(t *testing.T) {
	db, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := slog.New(slog.DiscardHandler)

	if s, set := LoadScheduler(t.Context(), db, "missing", scheduler.Deps{}, log); s.Name() != scheduler.LeastInflight || set.Strategy != scheduler.LeastInflight {
		t.Fatalf("bad fallback gave %s / %+v", s.Name(), set)
	}
	if s, _ := LoadScheduler(t.Context(), db, testStrategy, scheduler.Deps{}, log); s.Name() != testStrategy {
		t.Fatalf("configured fallback ignored: %s", s.Name())
	}
	if err := store.PutSetting(t.Context(), db, store.SettingScheduler, SchedulerSetting{Strategy: "gone"}); err != nil {
		t.Fatal(err)
	}
	if s, _ := LoadScheduler(t.Context(), db, testStrategy, scheduler.Deps{}, log); s.Name() != testStrategy {
		t.Fatalf("an unknown persisted strategy must fall back to the configured one, got %s", s.Name())
	}
	if err := store.PutSetting(t.Context(), db, store.SettingScheduler, SchedulerSetting{Strategy: scheduler.LeastInflight}); err != nil {
		t.Fatal(err)
	}
	if s, _ := LoadScheduler(t.Context(), db, testStrategy, scheduler.Deps{}, log); s.Name() != scheduler.LeastInflight {
		t.Fatalf("persisted setting ignored: %s", s.Name())
	}
}

func TestAdminGate(t *testing.T) {
	e := newEnv(t, &auth.User{Name: "alice", Roles: []string{"Application_Provider"}})

	me := e.want(e.do(http.MethodGet, "/me", ""), http.StatusOK)
	if me["name"] != "alice" || me["admin"] != false {
		t.Fatalf("me = %v", me)
	}
	for _, path := range []string{"/overview", "/models", "/nodes", "/deployments", "/keys", "/settings"} {
		e.want(e.do(http.MethodGet, path, ""), http.StatusOK)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/models", `{"name":"m","reference":"r","memory_mb":1}`},
		{http.MethodPatch, "/models/m_1", `{"auto":false}`},
		{http.MethodDelete, "/models/m_1", ""},
		{http.MethodPost, "/deployments", `{"model_id":"m","cluster":"c1","node":"good"}`},
		{http.MethodDelete, "/deployments/d_1", ""},
		{http.MethodPost, "/deployments/d_1/retry", ""},
		{http.MethodPost, "/keys", `{"name":"k"}`},
		{http.MethodDelete, "/keys/k_1", ""},
		{http.MethodPut, "/settings", `{"scheduler":{"strategy":"least-inflight","config":null}}`},
	} {
		if body := e.want(e.do(c.method, c.path, c.body), http.StatusForbidden); errCode(body) != "forbidden" {
			t.Errorf("%s %s = %v", c.method, c.path, body)
		}
	}

	e.token = ""
	if body := e.want(e.do(http.MethodGet, "/me", ""), http.StatusUnauthorized); errCode(body) != "unauthorized" {
		t.Fatalf("no token = %v", body)
	}
	e.token = jwt("mallory")
	e.want(e.do(http.MethodGet, "/me", ""), http.StatusUnauthorized)
}

func TestUnknownRouteIsJSON404(t *testing.T) {
	e := newEnv(t, nil)
	if body := e.want(e.do(http.MethodGet, "/nope", ""), http.StatusNotFound); errCode(body) != "not_found" {
		t.Fatalf("body = %v", body)
	}
}
