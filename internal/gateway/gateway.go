package gateway

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/scheduler"
)

// AutoModel is the model name that lets the scheduler choose.
const AutoModel = "auto"

// DefaultRankTimeout is how long a strategy may take to rank candidates
// before the gateway falls back to least-inflight.
const DefaultRankTimeout = 2 * time.Second

// Endpoint is one routable (ready) deployment.
type Endpoint struct {
	DeploymentID string
	ModelID      string
	// ModelName is the public model name and also the name the engine
	// serves, so request bodies are rewritten to it and responses need no
	// rewriting.
	ModelName string
	// Auto marks the model as eligible for "auto".
	Auto   bool
	Labels map[string]string

	Cluster, Node string
	// BaseURL is the engine's address without a trailing path, for example
	// http://10.0.0.5:18000.
	BaseURL   string
	EngineKey string
}

// Authenticator checks an API key secret.
type Authenticator interface {
	Authenticate(ctx context.Context, secret string) bool
}

// Options configures a Gateway.
type Options struct {
	// Auth is required.
	Auth Authenticator
	// MaxAttempts is the number of candidates tried per request (minimum 1).
	MaxAttempts int
	// MaxBodyBytes caps request bodies; 0 means 16 MiB.
	MaxBodyBytes int64
	// DebugHeaders adds X-OOA-Deployment, X-OOA-Node and X-OOA-Model to
	// responses. Off by default so clients learn nothing about topology.
	DebugHeaders bool
	Logger       *slog.Logger
	// Transport carries requests to engines. Nil uses a transport with a
	// short dial timeout and no overall timeout, because completions can run
	// for minutes.
	Transport http.RoundTripper
	// KnownModel tells a model that exists but has nothing ready (503) from
	// one that does not exist (404). Optional.
	KnownModel func(name string) bool
	// RankTimeout overrides DefaultRankTimeout.
	RankTimeout time.Duration
}

// Gateway serves the /v1 API.
type Gateway struct {
	opts      Options
	transport http.RoundTripper
	log       *slog.Logger
	started   time.Time

	state    atomic.Pointer[state]
	strategy atomic.Pointer[scheduler.Strategy]
	onFail   atomic.Pointer[func(string)]
}

// New returns a Gateway with no endpoints and the least-inflight strategy.
func New(o Options) *Gateway {
	if o.MaxAttempts < 1 {
		o.MaxAttempts = 1
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 16 << 20
	}
	if o.RankTimeout <= 0 {
		o.RankTimeout = DefaultRankTimeout
	}
	g := &Gateway{opts: o, transport: o.Transport, log: o.Logger, started: time.Now()}
	if g.log == nil {
		g.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if g.transport == nil {
		g.transport = &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		}
	}
	g.state.Store(&state{byModel: map[string][]*ep{}})
	if s, err := scheduler.New(scheduler.LeastInflight, nil, scheduler.Deps{}); err == nil {
		g.SetStrategy(s)
	}
	return g
}

// ep is an Endpoint plus the live counters of its deployment. The counters
// live in a separate allocation so they survive SetEndpoints, which replaces
// the Endpoint (the address may have changed) but not the deployment.
type ep struct {
	Endpoint
	*stats
}

type stats struct {
	inflight atomic.Int64
	ttftNS   atomic.Int64
	// failedAt is when a connection to the engine last failed (unix nanos).
	failedAt atomic.Int64
}

// failurePenalty is how long an engine that refused a connection is tried
// last. A dead engine has no requests in flight, so without this it would
// rank first again and cost every request a connect timeout until the
// health probe takes it out of rotation.
const failurePenalty = 15 * time.Second

func (s *stats) recentlyFailed(now time.Time) bool {
	at := s.failedAt.Load()
	return at != 0 && now.Sub(time.Unix(0, at)) < failurePenalty
}

// demoteFailed moves recently failed endpoints behind the others, keeping
// the ranked order within each group.
func demoteFailed(eps []*ep) []*ep {
	now := time.Now()
	out := make([]*ep, 0, len(eps))
	var failed []*ep
	for _, e := range eps {
		if e.recentlyFailed(now) {
			failed = append(failed, e)
		} else {
			out = append(out, e)
		}
	}
	return append(out, failed...)
}

// observeTTFT folds a sample into an exponentially weighted average, so one
// slow request does not dominate the estimate.
func (s *stats) observeTTFT(d time.Duration) {
	const alpha = 0.3
	for {
		old := s.ttftNS.Load()
		next := int64(d)
		if old > 0 {
			next = int64(alpha*float64(d) + (1-alpha)*float64(old))
		}
		next = max(next, 1)
		if s.ttftNS.CompareAndSwap(old, next) {
			return
		}
	}
}

type state struct {
	all     []*ep
	auto    []*ep
	byModel map[string][]*ep
}

// SetEndpoints replaces the routable set. Deployments present before and
// after keep their in-flight count and TTFT estimate.
func (g *Gateway) SetEndpoints(eps []Endpoint) {
	old := map[string]*stats{}
	for _, e := range g.state.Load().all {
		old[e.DeploymentID] = e.stats
	}
	st := &state{byModel: map[string][]*ep{}}
	for _, e := range eps {
		e.BaseURL = strings.TrimRight(e.BaseURL, "/")
		s := old[e.DeploymentID]
		if s == nil {
			s = &stats{}
			old[e.DeploymentID] = s // a duplicate id in one call shares counters
		}
		p := &ep{Endpoint: e, stats: s}
		st.all = append(st.all, p)
		st.byModel[e.ModelName] = append(st.byModel[e.ModelName], p)
		if e.Auto {
			st.auto = append(st.auto, p)
		}
	}
	g.state.Store(st)
}

// SetStrategy swaps the strategy used for "auto".
func (g *Gateway) SetStrategy(s scheduler.Strategy) {
	g.strategy.Store(&s)
}

// OnUpstreamFailure registers a callback invoked with the deployment id each
// time an engine could not be reached. It runs on the request goroutine, so
// it must not block.
func (g *Gateway) OnUpstreamFailure(f func(deploymentID string)) {
	g.onFail.Store(&f)
}

func (g *Gateway) failed(e *ep) {
	e.failedAt.Store(time.Now().UnixNano())
	if f := g.onFail.Load(); f != nil && *f != nil {
		(*f)(e.DeploymentID)
	}
}

func (g *Gateway) candidate(e *ep) scheduler.Candidate {
	return scheduler.Candidate{
		DeploymentID: e.DeploymentID,
		Model:        e.ModelName,
		Cluster:      e.Cluster,
		Node:         e.Node,
		Labels:       e.Labels,
		InFlight:     int(e.inflight.Load()),
		TTFT:         time.Duration(e.ttftNS.Load()),
	}
}

func (g *Gateway) candidates(eps []*ep) []scheduler.Candidate {
	out := make([]scheduler.Candidate, len(eps))
	for i, e := range eps {
		out[i] = g.candidate(e)
	}
	return out
}

// order maps ranked candidates back to endpoints, ignoring ids the
// strategy invented and repeats.
func order(eps []*ep, ranked []scheduler.Candidate) []*ep {
	byID := make(map[string]*ep, len(eps))
	for _, e := range eps {
		byID[e.DeploymentID] = e
	}
	var out []*ep
	for _, c := range ranked {
		if e, ok := byID[c.DeploymentID]; ok && !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

// rank orders eps for r. Explicit models always use least-inflight; "auto"
// asks the configured strategy under a time budget and falls back to
// least-inflight on error, timeout or an empty answer.
func (g *Gateway) rank(ctx context.Context, r scheduler.Request, eps []*ep, auto bool) []*ep {
	cands := g.candidates(eps)
	if !auto {
		return order(eps, scheduler.RankLeastInflight(cands))
	}
	strat := *g.strategy.Load()
	ranked, err := g.runStrategy(ctx, strat, r, cands)
	if err == nil {
		if out := order(eps, ranked); len(out) > 0 {
			return out
		}
		err = errNoCandidates
	}
	g.log.WarnContext(ctx, "scheduler strategy failed, using least-inflight",
		"strategy", strat.Name(), "err", err)
	return order(eps, scheduler.RankLeastInflight(cands))
}

type strategyError string

func (e strategyError) Error() string { return string(e) }

const errNoCandidates = strategyError("strategy returned no usable candidates")

// runStrategy enforces the time budget even for a strategy that ignores its
// context, and keeps a panicking strategy from taking the request down.
func (g *Gateway) runStrategy(ctx context.Context, s scheduler.Strategy, r scheduler.Request, c []scheduler.Candidate) ([]scheduler.Candidate, error) {
	ctx, cancel := context.WithTimeout(ctx, g.opts.RankTimeout)
	defer cancel()
	type result struct {
		c   []scheduler.Candidate
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				ch <- result{err: strategyError("strategy panicked")}
			}
		}()
		out, err := s.Rank(ctx, r, c)
		ch <- result{out, err}
	}()
	select {
	case res := <-ch:
		return res.c, res.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}
