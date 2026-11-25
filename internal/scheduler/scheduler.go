package scheduler

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

// Message is one chat message reduced to text.
type Message struct {
	Role, Content string
}

// Request describes the call a strategy is asked to place.
type Request struct {
	// API is "chat.completions", "completions" or "embeddings".
	API    string
	Stream bool
	// Tools is true when the request declares tools.
	Tools bool
	// ApproxTokens is a rough estimate of the prompt size, good enough to
	// tell a one-liner from a long document.
	ApproxTokens int
	// MaxTokens is max_completion_tokens or max_tokens, 0 when unset.
	MaxTokens int
	Messages  []Message
	// Prompt is the text of a legacy completions request.
	Prompt string
	// Hints come from X-OOA-* request headers (lowercased name without the
	// prefix) and the body's "metadata" object. Headers win on conflicts.
	Hints map[string]string
	// Raw is the parsed request body. Treat it as read-only.
	Raw map[string]json.RawMessage
}

// Candidate is one ready deployment a request could be sent to.
type Candidate struct {
	DeploymentID, Model, Cluster, Node string
	// Labels are the model's labels.
	Labels   map[string]string
	InFlight int
	// TTFT is the recent time to response headers, 0 when unknown.
	TTFT time.Duration
}

// Strategy orders candidates for a request.
type Strategy interface {
	Name() string
	// Rank returns the candidates best first and may drop some. It must not
	// modify c.
	Rank(ctx context.Context, r Request, c []Candidate) ([]Candidate, error)
}

// Deps is what the server hands to a strategy factory.
type Deps struct {
	// Complete runs a non-streaming chat completion against a deployed
	// model, so a strategy can consult a decision model. May be nil in tests.
	Complete func(ctx context.Context, model string, msgs []Message) (string, error)
	Logger   *slog.Logger
}

// Factory builds a strategy from its JSON config (may be empty or null).
type Factory func(cfg json.RawMessage, d Deps) (Strategy, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes a strategy available under name. It panics on an empty
// name, a nil factory or a duplicate, because those are programming errors
// caught at startup.
func Register(name string, f Factory) {
	if name == "" || f == nil {
		panic("scheduler: Register needs a name and a factory")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic(fmt.Sprintf("scheduler: strategy %q registered twice", name))
	}
	factories[name] = f
}

// New builds the strategy registered under name.
func New(name string, cfg json.RawMessage, d Deps) (Strategy, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("scheduler: unknown strategy %q (available: %v)", name, Names())
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return f(cfg, d)
}

// Names lists the registered strategies, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	return slices.Sorted(maps.Keys(factories))
}

// LeastInflight is the name of the built-in default strategy.
const LeastInflight = "least-inflight"

func init() {
	Register(LeastInflight, func(json.RawMessage, Deps) (Strategy, error) {
		return leastInflightStrategy{}, nil
	})
}

type leastInflightStrategy struct{}

func (leastInflightStrategy) Name() string { return LeastInflight }

func (leastInflightStrategy) Rank(_ context.Context, _ Request, c []Candidate) ([]Candidate, error) {
	return RankLeastInflight(c), nil
}

// RankLeastInflight returns a copy of c ordered by fewest in-flight
// requests, then by lower known TTFT, then randomly. Other strategies use it
// to order a subset they selected.
func RankLeastInflight(c []Candidate) []Candidate {
	out := slices.Clone(c)
	// Shuffle first and sort stably so candidates that tie on everything end
	// up in random order instead of always favouring the first deployment.
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	slices.SortStableFunc(out, func(a, b Candidate) int {
		if a.InFlight != b.InFlight {
			return a.InFlight - b.InFlight
		}
		// A known TTFT beats an unknown one so the order stays a consistent
		// total preorder; an unknown value says nothing about speed.
		switch {
		case a.TTFT > 0 && b.TTFT > 0:
			return cmp.Compare(a.TTFT, b.TTFT)
		case a.TTFT > 0:
			return -1
		case b.TTFT > 0:
			return 1
		}
		return 0
	})
	return out
}
