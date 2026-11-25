package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/scheduler"
)

const clientKey = "sk-ooa-client"

type keyAuth map[string]bool

func (k keyAuth) Authenticate(_ context.Context, s string) bool { return k[s] }

// engine is a fake upstream that records what it received.
type engine struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  []map[string]any
	headers []http.Header
	paths   []string
	hits    atomic.Int32
}

func newEngine(t *testing.T, h http.HandlerFunc) *engine {
	t.Helper()
	e := &engine{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		e.mu.Lock()
		e.bodies = append(e.bodies, m)
		e.headers = append(e.headers, r.Header.Clone())
		e.paths = append(e.paths, r.URL.RequestURI())
		e.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *engine) last() (body map[string]any, hdr http.Header, uri string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	i := len(e.bodies) - 1
	return e.bodies[i], e.headers[i], e.paths[i]
}

func newGateway(t *testing.T, o Options) *Gateway {
	t.Helper()
	o.Auth = keyAuth{clientKey: true}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = 3
	}
	return New(o)
}

func endpointFor(id, model string, e *engine) Endpoint {
	return Endpoint{DeploymentID: id, ModelID: "m_" + model, ModelName: model, Auto: true, Node: "node-" + id,
		Cluster: "c1", BaseURL: e.URL, EngineKey: "engine-" + id}
}

func do(t *testing.T, g *Gateway, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+clientKey)
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			r.Header.Del(hdr[i])
		} else {
			r.Header.Set(hdr[i], hdr[i+1])
		}
	}
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, r)
	return rec
}

func errBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || m.Error == nil {
		t.Fatalf("not an OpenAI error: %s", rec.Body)
	}
	for _, k := range []string{"message", "type", "param", "code"} {
		if _, ok := m.Error[k]; !ok {
			t.Fatalf("error lacks %q: %s", k, rec.Body)
		}
	}
	return m.Error
}

const chatBody = `{"model":"llama","messages":[{"role":"user","content":"hello"}]}`

func TestExplicitModelRoutingAndRewrite(t *testing.T) {
	a, b := newEngine(t, nil), newEngine(t, nil)
	g := newGateway(t, Options{})
	g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a), endpointFor("d2", "other", b)})

	rec := do(t, g, "POST", "/v1/chat/completions?foo=bar", chatBody, "X-Custom", "yes", "X-OOA-Tier", "gold")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"hi"`) {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if a.hits.Load() != 1 || b.hits.Load() != 0 {
		t.Fatalf("hits a=%d b=%d", a.hits.Load(), b.hits.Load())
	}
	body, hdr, uri := a.last()
	if body["model"] != "llama" || uri != "/v1/chat/completions?foo=bar" {
		t.Fatalf("body=%v uri=%s", body, uri)
	}
	if hdr.Get("Authorization") != "Bearer engine-d1" {
		t.Fatalf("engine got Authorization %q", hdr.Get("Authorization"))
	}
	for k, vs := range hdr {
		for _, v := range vs {
			if strings.Contains(v, clientKey) {
				t.Fatalf("client key leaked in %s", k)
			}
		}
		if strings.HasPrefix(k, "X-Ooa-") {
			t.Fatalf("hint header %s reached the engine", k)
		}
	}
	if hdr.Get("X-Custom") != "yes" {
		t.Fatal("end-to-end header dropped")
	}
	for k := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(k), "x-ooa-") {
			t.Fatalf("response header %s leaked", k)
		}
	}
}

func TestModelRewriteForAuto(t *testing.T) {
	a := newEngine(t, nil)
	g := newGateway(t, Options{})
	g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a)})
	if rec := do(t, g, "POST", "/v1/chat/completions", `{"model":"auto","messages":[]}`); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if body, _, _ := a.last(); body["model"] != "llama" {
		t.Fatalf("model = %v", body["model"])
	}
}

func TestDebugHeaders(t *testing.T) {
	a := newEngine(t, nil)
	g := newGateway(t, Options{DebugHeaders: true})
	g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a)})
	rec := do(t, g, "POST", "/v1/chat/completions", chatBody)
	if rec.Header().Get("X-OOA-Deployment") != "d1" || rec.Header().Get("X-OOA-Node") != "node-d1" || rec.Header().Get("X-OOA-Model") != "llama" {
		t.Fatalf("headers %v", rec.Header())
	}
}

type fnStrategy func(context.Context, scheduler.Request, []scheduler.Candidate) ([]scheduler.Candidate, error)

func (fnStrategy) Name() string { return "test" }
func (f fnStrategy) Rank(ctx context.Context, r scheduler.Request, c []scheduler.Candidate) ([]scheduler.Candidate, error) {
	return f(ctx, r, c)
}

func TestAutoUsesStrategyAndFallsBack(t *testing.T) {
	a, b := newEngine(t, nil), newEngine(t, nil)
	g := newGateway(t, Options{RankTimeout: 100 * time.Millisecond})
	g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a), endpointFor("d2", "other", b)})

	var seen scheduler.Request
	pickD2 := fnStrategy(func(_ context.Context, r scheduler.Request, c []scheduler.Candidate) ([]scheduler.Candidate, error) {
		seen = r
		for _, x := range c {
			if x.DeploymentID == "d2" {
				return []scheduler.Candidate{x}, nil
			}
		}
		return nil, nil
	})
	g.SetStrategy(pickD2)
	rec := do(t, g, "POST", "/v1/chat/completions",
		`{"model":"auto","stream":false,"max_completion_tokens":5,"tools":[{"type":"function"}],"metadata":{"tier":"gold"},`+
			`"messages":[{"role":"user","content":[{"type":"text","text":"abcd"},{"type":"image_url","image_url":{}}]}]}`,
		"X-OOA-Role", "coder")
	if rec.Code != 200 || b.hits.Load() != 1 || a.hits.Load() != 0 {
		t.Fatalf("code=%d a=%d b=%d", rec.Code, a.hits.Load(), b.hits.Load())
	}
	if seen.API != "chat.completions" || !seen.Tools || seen.MaxTokens != 5 || seen.ApproxTokens != 1 ||
		len(seen.Messages) != 1 || seen.Messages[0].Content != "abcd" ||
		seen.Hints["tier"] != "gold" || seen.Hints["role"] != "coder" || seen.Raw["model"] == nil {
		t.Fatalf("request = %+v", seen)
	}

	cases := map[string]fnStrategy{
		"error": func(context.Context, scheduler.Request, []scheduler.Candidate) ([]scheduler.Candidate, error) {
			return nil, errors.New("boom")
		},
		"empty": func(context.Context, scheduler.Request, []scheduler.Candidate) ([]scheduler.Candidate, error) {
			return nil, nil
		},
		"timeout": func(ctx context.Context, _ scheduler.Request, _ []scheduler.Candidate) ([]scheduler.Candidate, error) {
			time.Sleep(time.Second) // ignores ctx on purpose
			return nil, nil
		},
		"panic": func(context.Context, scheduler.Request, []scheduler.Candidate) ([]scheduler.Candidate, error) {
			panic("bad strategy")
		},
		"unknown ids": func(context.Context, scheduler.Request, []scheduler.Candidate) ([]scheduler.Candidate, error) {
			return []scheduler.Candidate{{DeploymentID: "ghost"}}, nil
		},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			g.SetStrategy(s)
			before := a.hits.Load() + b.hits.Load()
			start := time.Now()
			if rec := do(t, g, "POST", "/v1/chat/completions", `{"model":"auto","messages":[]}`); rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if a.hits.Load()+b.hits.Load() != before+1 {
				t.Fatal("request was not served by the fallback")
			}
			if time.Since(start) > 900*time.Millisecond {
				t.Fatal("fallback did not respect the rank budget")
			}
		})
	}
}

func TestExplicitModelIgnoresStrategy(t *testing.T) {
	a := newEngine(t, nil)
	g := newGateway(t, Options{})
	g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a)})
	g.SetStrategy(fnStrategy(func(context.Context, scheduler.Request, []scheduler.Candidate) ([]scheduler.Candidate, error) {
		t.Error("strategy consulted for an explicit model")
		return nil, nil
	}))
	if rec := do(t, g, "POST", "/v1/chat/completions", chatBody); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
}

func TestSSEPassthroughIsIncremental(t *testing.T) {
	release := make(chan struct{})
	a := newEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "9999")
		_, _ = io.WriteString(w, "data: one\n\n")
		_ = http.NewResponseController(w).Flush()
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	g := newGateway(t, Options{})
	g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a)})
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"llama","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+clientKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("X-Accel-Buffering") != "no" || resp.ContentLength != -1 {
		t.Fatalf("headers %v len %d", resp.Header, resp.ContentLength)
	}
	r := bufio.NewReader(resp.Body)
	line, err := r.ReadString('\n')
	if err != nil || line != "data: one\n" {
		t.Fatalf("first chunk %q, %v (upstream not finished yet)", line, err)
	}
	if n := g.state.Load().all[0].inflight.Load(); n != 1 {
		t.Fatalf("inflight during stream = %d", n)
	}
	close(release)
	rest, _ := io.ReadAll(r)
	if !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("rest %q", rest)
	}
	waitInflightZero(t, g)
}

func waitInflightZero(t *testing.T, g *Gateway) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		zero := true
		for _, e := range g.state.Load().all {
			zero = zero && e.inflight.Load() == 0
		}
		if zero {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("in-flight counter did not return to zero")
}

func deadURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + l.Addr().String()
	_ = l.Close()
	return u
}

func TestRetryOnConnectionFailure(t *testing.T) {
	good := newEngine(t, nil)
	g := newGateway(t, Options{})
	dead := Endpoint{DeploymentID: "dead", ModelName: "llama", Auto: true, BaseURL: deadURL(t)}
	g.SetEndpoints([]Endpoint{dead, endpointFor("ok", "llama", good)})
	// Make the dead one rank first.
	g.state.Load().byModel["llama"][1].inflight.Store(5)

	var mu sync.Mutex
	var failed []string
	g.OnUpstreamFailure(func(id string) { mu.Lock(); failed = append(failed, id); mu.Unlock() })

	rec := do(t, g, "POST", "/v1/chat/completions", chatBody)
	if rec.Code != 200 || good.hits.Load() != 1 {
		t.Fatalf("code %d hits %d", rec.Code, good.hits.Load())
	}
	if len(failed) != 1 || failed[0] != "dead" {
		t.Fatalf("failed = %v", failed)
	}
	g.state.Load().byModel["llama"][1].inflight.Store(0) // the artificial load from above
	waitInflightZero(t, g)
}

func TestNoRetryAfterEngineAnswered(t *testing.T) {
	a := newEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"engine broke"}`)
	})
	b := newEngine(t, nil)
	g := newGateway(t, Options{})
	g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a), endpointFor("d2", "llama", b)})
	g.state.Load().byModel["llama"][1].inflight.Store(5)
	called := false
	g.OnUpstreamFailure(func(string) { called = true })
	rec := do(t, g, "POST", "/v1/chat/completions", chatBody)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "engine broke") || b.hits.Load() != 0 || called {
		t.Fatalf("code %d body %s b=%d called=%v", rec.Code, rec.Body, b.hits.Load(), called)
	}
}

func TestAllAttemptsFail(t *testing.T) {
	g := newGateway(t, Options{MaxAttempts: 2})
	g.SetEndpoints([]Endpoint{
		{DeploymentID: "x1", ModelName: "llama", BaseURL: deadURL(t)},
		{DeploymentID: "x2", ModelName: "llama", BaseURL: deadURL(t)},
		{DeploymentID: "x3", ModelName: "llama", BaseURL: deadURL(t)},
	})
	n := 0
	g.OnUpstreamFailure(func(string) { n++ })
	rec := do(t, g, "POST", "/v1/chat/completions", chatBody)
	if rec.Code != 502 || errBody(t, rec)["code"] != "upstream_unreachable" || n != 2 {
		t.Fatalf("code %d n=%d body %s", rec.Code, n, rec.Body)
	}
}

func TestErrorShapes(t *testing.T) {
	a := newEngine(t, nil)
	g := newGateway(t, Options{MaxBodyBytes: 64})
	g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a), {DeploymentID: "d2", ModelName: "plain", BaseURL: a.URL}})
	g.opts.KnownModel = func(n string) bool { return n == "known-not-ready" }

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		hdr    []string
		status int
		code   string
	}{
		{"no key", "POST", "/v1/chat/completions", chatBody, []string{"Authorization", ""}, 401, "invalid_api_key"},
		{"bad key", "GET", "/v1/models", "", []string{"Authorization", "Bearer nope"}, 401, "invalid_api_key"},
		{"basic scheme", "GET", "/v1/models", "", []string{"Authorization", "Basic " + clientKey}, 401, "invalid_api_key"},
		{"unknown model", "POST", "/v1/chat/completions", `{"model":"zzz"}`, nil, 404, "model_not_found"},
		{"known but not ready", "POST", "/v1/chat/completions", `{"model":"known-not-ready"}`, nil, 503, "no_deployment_available"},
		{"too large", "POST", "/v1/chat/completions", `{"model":"llama","pad":"` + strings.Repeat("x", 100) + `"}`, nil, 413, "request_too_large"},
		{"bad json", "POST", "/v1/chat/completions", `{`, nil, 400, "invalid_json"},
		{"not object", "POST", "/v1/chat/completions", `[]`, nil, 400, "invalid_json"},
		{"missing model", "POST", "/v1/chat/completions", `{"messages":[]}`, nil, 400, "missing_required_parameter"},
		{"unknown model route", "GET", "/v1/models/zzz", "", nil, 404, "model_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, g, tt.method, tt.path, tt.body, tt.hdr...)
			if rec.Code != tt.status || errBody(t, rec)["code"] != tt.code {
				t.Fatalf("status %d body %s", rec.Code, rec.Body)
			}
		})
	}

	empty := newGateway(t, Options{})
	rec := do(t, empty, "POST", "/v1/chat/completions", `{"model":"auto"}`)
	if rec.Code != 503 || errBody(t, rec)["code"] != "no_deployment_available" {
		t.Fatalf("auto with nothing: %d %s", rec.Code, rec.Body)
	}
	empty.SetEndpoints([]Endpoint{{DeploymentID: "d", ModelName: "m", Auto: false, BaseURL: a.URL}})
	if rec := do(t, empty, "POST", "/v1/chat/completions", `{"model":"auto"}`); rec.Code != 503 {
		t.Fatalf("auto with only non-auto models: %d", rec.Code)
	}
}

func TestModels(t *testing.T) {
	a := newEngine(t, nil)
	g := newGateway(t, Options{})
	g.SetEndpoints([]Endpoint{
		{DeploymentID: "d1", ModelName: "zeta", Auto: true, BaseURL: a.URL},
		{DeploymentID: "d2", ModelName: "zeta", Auto: true, BaseURL: a.URL},
		{DeploymentID: "d3", ModelName: "alpha", BaseURL: a.URL},
	})
	rec := do(t, g, "GET", "/v1/models", "")
	var list struct {
		Object string
		Data   []modelObject
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != 200 {
		t.Fatal(err, rec.Body)
	}
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID)
		if m.Object != "model" || m.OwnedBy != "oakestra" || m.Created == 0 {
			t.Fatalf("model %+v", m)
		}
	}
	if list.Object != "list" || strings.Join(ids, ",") != "alpha,zeta,auto" {
		t.Fatalf("list %s", rec.Body)
	}

	rec = do(t, g, "GET", "/v1/models/auto", "")
	var one modelObject
	_ = json.Unmarshal(rec.Body.Bytes(), &one)
	if rec.Code != 200 || one.ID != "auto" || one.Object != "model" {
		t.Fatalf("single %d %s", rec.Code, rec.Body)
	}

	g.SetEndpoints([]Endpoint{{DeploymentID: "d3", ModelName: "alpha", BaseURL: a.URL}})
	if strings.Contains(do(t, g, "GET", "/v1/models", "").Body.String(), "auto") {
		t.Fatal("auto listed without auto endpoints")
	}
	g.SetEndpoints(nil)
	if rec := do(t, g, "GET", "/v1/models", ""); !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("empty list %s", rec.Body)
	}
}

func TestSetEndpointsKeepsCounters(t *testing.T) {
	a := newEngine(t, nil)
	g := newGateway(t, Options{})
	g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a), endpointFor("d2", "llama", a)})
	old := g.state.Load().byModel["llama"][0]
	old.inflight.Store(3)
	old.observeTTFT(40 * time.Millisecond)

	g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a), endpointFor("d3", "llama", a)})
	var d1, d3 *ep
	for _, e := range g.state.Load().all {
		switch e.DeploymentID {
		case "d1":
			d1 = e
		case "d3":
			d3 = e
		}
	}
	if d1.inflight.Load() != 3 || d1.ttftNS.Load() != int64(40*time.Millisecond) || d3.inflight.Load() != 0 {
		t.Fatalf("d1=%d/%d d3=%d", d1.inflight.Load(), d1.ttftNS.Load(), d3.inflight.Load())
	}
	c := g.candidate(d1)
	if c.InFlight != 3 || c.TTFT != 40*time.Millisecond || c.Model != "llama" || c.Node != "node-d1" {
		t.Fatalf("candidate %+v", c)
	}
	d1.observeTTFT(140 * time.Millisecond)
	if got := time.Duration(d1.ttftNS.Load()); got != 70*time.Millisecond {
		t.Fatalf("ewma = %v", got)
	}
}

func TestConcurrentSetEndpoints(t *testing.T) {
	a := newEngine(t, nil)
	g := newGateway(t, Options{})
	g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a)})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a), endpointFor("d2", "llama", a)})
			} else {
				g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a)})
			}
			g.SetStrategy(fnStrategy(func(_ context.Context, _ scheduler.Request, c []scheduler.Candidate) ([]scheduler.Candidate, error) {
				return c, nil
			}))
		}
	})
	var reqs sync.WaitGroup
	for range 8 {
		reqs.Go(func() {
			for range 25 {
				for _, model := range []string{"llama", "auto"} {
					rec := do(t, g, "POST", "/v1/chat/completions", `{"model":"`+model+`"}`)
					if rec.Code != 200 {
						t.Errorf("status %d: %s", rec.Code, rec.Body)
						return
					}
				}
			}
		})
	}
	reqs.Wait()
	close(stop)
	wg.Wait()
	waitInflightZero(t, g)
}

func TestClientCancelCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	a := newEngine(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: x\n\n")
		_ = http.NewResponseController(w).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	g := newGateway(t, Options{})
	g.SetEndpoints([]Endpoint{endpointFor("d1", "llama", a)})
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Authorization", "Bearer "+clientKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	_ = resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request not cancelled")
	}
	waitInflightZero(t, g)
}

func TestComplete(t *testing.T) {
	a := newEngine(t, nil)
	g := newGateway(t, Options{})
	g.SetEndpoints([]Endpoint{endpointFor("d1", "judge", a)})
	got, err := g.Complete(t.Context(), "judge", []scheduler.Message{{Role: "user", Content: "easy or hard?"}})
	if err != nil || got != "hi" {
		t.Fatalf("got %q, %v", got, err)
	}
	body, hdr, uri := a.last()
	if body["model"] != "judge" || body["stream"] != false || uri != "/v1/chat/completions" || hdr.Get("Authorization") != "Bearer engine-d1" {
		t.Fatalf("body=%v uri=%s", body, uri)
	}
	if _, err := g.Complete(t.Context(), "missing", nil); err == nil {
		t.Fatal("expected error for a model without deployment")
	}
	bad := newEngine(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	g.SetEndpoints([]Endpoint{endpointFor("d2", "judge", bad)})
	if _, err := g.Complete(t.Context(), "judge", nil); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
	waitInflightZero(t, g)
}

func TestEmbeddingsAndCompletionsRequest(t *testing.T) {
	r := buildRequest("completions", map[string]json.RawMessage{
		"prompt": json.RawMessage(`["abcd","efgh"]`), "max_tokens": json.RawMessage(`7`),
		"tools": json.RawMessage(`[]`), "metadata": json.RawMessage(`{"n":3,"s":"x"}`),
	}, http.Header{"X-Ooa-Tier": {"a"}})
	if r.Prompt != "abcd\nefgh" || r.MaxTokens != 7 || r.Tools || r.Hints["n"] != "3" || r.Hints["s"] != "x" || r.Hints["tier"] != "a" {
		t.Fatalf("%+v", r)
	}
	e := buildRequest("embeddings", map[string]json.RawMessage{"input": json.RawMessage(`"hello world"`)}, nil)
	if e.Prompt != "hello world" || e.ApproxTokens != 3 {
		t.Fatalf("%+v", e)
	}
}

// An engine that just refused a connection is tried last, even though it
// has nothing in flight and would otherwise rank first again.
func TestFailedEndpointIsTriedLast(t *testing.T) {
	good := newEngine(t, nil)
	g := newGateway(t, Options{})
	dead := Endpoint{DeploymentID: "dead", ModelName: "llama", Auto: true, BaseURL: deadURL(t)}
	g.SetEndpoints([]Endpoint{dead, endpointFor("ok", "llama", good)})
	g.state.Load().byModel["llama"][1].inflight.Store(5)

	var failures atomic.Int64
	g.OnUpstreamFailure(func(string) { failures.Add(1) })

	for range 3 {
		if rec := do(t, g, "POST", "/v1/chat/completions", chatBody); rec.Code != 200 {
			t.Fatalf("code %d", rec.Code)
		}
	}
	if n := failures.Load(); n != 1 {
		t.Fatalf("dead engine tried %d times, want 1", n)
	}
	g.state.Load().byModel["llama"][1].inflight.Store(0)
	waitInflightZero(t, g)
}
