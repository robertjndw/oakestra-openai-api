package deploy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/config"
	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra/oakestratest"
	"github.com/robertjndw/oakestra-openai-api/internal/store"
)

// harness wires a Manager to the fake Oakestra through the real HTTP
// client, with a manual clock and fake engines that the fake starts on
// deploy. Engines listen on random ports; the probe client's dialer maps
// the node ip:host port the Manager dials to the engine's real address.
type harness struct {
	t       *testing.T
	cfg     config.Config
	db      *store.DB
	fake    *oakestratest.Fake
	oak     *oakestra.HTTPClient
	m       *Manager
	engines *engines

	// down makes the fake drop every connection, like an unreachable
	// system manager.
	down atomic.Bool

	clockMu sync.Mutex
	clock   time.Time
}

func newHarness(t *testing.T, tweak func(*config.Config)) *harness {
	t.Helper()
	cfg, err := config.LoadFrom(map[string]string{
		"OOA_OAKESTRA_USER":     "svc",
		"OOA_OAKESTRA_PASSWORD": "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tweak != nil {
		tweak(&cfg)
	}
	h := &harness{t: t, cfg: cfg, engines: newEngines(t), clock: time.UnixMilli(1_800_000_000_000)}

	h.fake = oakestratest.New(oakestratest.Options{
		OnDeploy:   h.engines.start,
		OnUndeploy: h.engines.stop,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.down.Load() {
			panic(http.ErrAbortHandler)
		}
		h.fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	opts := oakestratest.ClientOptions(srv.URL, "svc", "pw")
	opts.Timeout = 5 * time.Second
	opts.Logger = slog.New(slog.DiscardHandler)
	h.oak, err = oakestra.New(opts)
	if err != nil {
		t.Fatal(err)
	}

	h.db, err = store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.db.Close() })

	h.m, err = New(Options{
		Config:      cfg,
		DB:          h.db,
		Oakestra:    h.oak,
		ProbeClient: h.engines.client(),
		Now:         h.now,
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	h.fake.AddCluster("c1", "10.0.0.1")
	h.addNode("c1", oakestratest.NodeSpec{Name: "n1"})
	return h
}

func (h *harness) now() time.Time {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	return h.clock
}

func (h *harness) advance(d time.Duration) {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	h.clock = h.clock.Add(d)
}

func (h *harness) addNode(cluster string, spec oakestratest.NodeSpec) {
	h.t.Helper()
	if _, err := h.fake.AddNode(cluster, spec); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) model(name string) Model {
	h.t.Helper()
	mod, err := h.m.CreateModel(h.t.Context(), ModelInput{Name: name, Reference: "registry.example/" + name + ":1", MemoryMB: 1024})
	if err != nil {
		h.t.Fatalf("CreateModel(%s): %v", name, err)
	}
	return mod
}

func (h *harness) deploy(mod Model, cluster, node string) Deployment {
	h.t.Helper()
	d, err := h.m.CreateDeployment(h.t.Context(), mod.ID, cluster, node)
	if err != nil {
		h.t.Fatalf("CreateDeployment(%s on %s/%s): %v", mod.Name, cluster, node, err)
	}
	return d
}

func (h *harness) tick() { h.m.tick(h.t.Context()) }

func (h *harness) row(id string) row {
	h.t.Helper()
	r, err := h.m.getRow(h.t.Context(), id)
	if err != nil {
		h.t.Fatalf("row %s: %v", id, err)
	}
	return r
}

func (h *harness) gone(id string) bool {
	h.t.Helper()
	_, err := h.m.getRow(h.t.Context(), id)
	return wantStatus(err, http.StatusNotFound)
}

func (h *harness) wantState(id string, want State) row {
	h.t.Helper()
	r := h.row(id)
	if r.State != want {
		h.t.Fatalf("deployment %s state = %s (error %q), want %s", id, r.State, r.Error, want)
	}
	return r
}

// toReady walks a fresh deployment through the three steps the fake needs.
func (h *harness) toReady(id string) {
	h.t.Helper()
	for _, want := range []State{StateScheduling, StateStarting, StateReady} {
		h.tick()
		h.wantState(id, want)
	}
}

func wantStatus(err error, status int) bool {
	he, ok := errors.AsType[*httpx.Error](err)
	return ok && he.Status == status
}

func assertStatus(t *testing.T, err error, status int) {
	t.Helper()
	if !wantStatus(err, status) {
		t.Fatalf("err = %v, want an httpx error with status %d", err, status)
	}
}

// engines are fake vLLM servers keyed by the address the Manager dials.
type engines struct {
	t  *testing.T
	mu sync.Mutex
	m  map[string]*engine
}

type engine struct {
	key     string
	cmd     []string
	healthy atomic.Bool
	srv     *httptest.Server
}

func newEngines(t *testing.T) *engines {
	e := &engines{t: t, m: map[string]*engine{}}
	t.Cleanup(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, en := range e.m {
			en.srv.Close()
		}
	})
	return e
}

func (e *engines) start(_ context.Context, d oakestratest.Deployment) error {
	en := &engine{cmd: d.Cmd}
	if i := slices.Index(d.Cmd, "--api-key"); i >= 0 && i+1 < len(d.Cmd) {
		en.key = d.Cmd[i+1]
	}
	en.healthy.Store(true)
	en.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !en.healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/health":
		case "/v1/models":
			if r.Header.Get("Authorization") != "Bearer "+en.key {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	addr := net.JoinHostPort(d.Node.IP, strconv.Itoa(d.HostPort))
	e.mu.Lock()
	defer e.mu.Unlock()
	if old := e.m[addr]; old != nil {
		old.srv.Close()
	}
	e.m[addr] = en
	return nil
}

func (e *engines) stop(d oakestratest.Deployment) {
	addr := net.JoinHostPort(d.Node.IP, strconv.Itoa(d.HostPort))
	e.mu.Lock()
	defer e.mu.Unlock()
	if en := e.m[addr]; en != nil {
		en.srv.Close()
		delete(e.m, addr)
	}
}

func (e *engines) at(addr string) *engine {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.m[addr]
}

func (e *engines) client() *http.Client {
	var d net.Dialer
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				en := e.at(addr)
				if en == nil {
					return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
				}
				return d.DialContext(ctx, network, en.srv.Listener.Addr().String())
			},
		},
	}
}
