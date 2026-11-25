package scheduler

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func ids(c []Candidate) []string {
	out := make([]string, len(c))
	for i, x := range c {
		out[i] = x.DeploymentID
	}
	return out
}

func rank(t *testing.T, c []Candidate) []Candidate {
	t.Helper()
	s, err := New(LeastInflight, nil, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Rank(t.Context(), Request{}, c)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLeastInflightOrdersByInflight(t *testing.T) {
	in := []Candidate{{DeploymentID: "a", InFlight: 3}, {DeploymentID: "b", InFlight: 0}, {DeploymentID: "c", InFlight: 1}}
	if got := ids(rank(t, in)); !slices.Equal(got, []string{"b", "c", "a"}) {
		t.Fatalf("order = %v", got)
	}
	if in[0].DeploymentID != "a" {
		t.Fatal("input was modified")
	}
}

func TestLeastInflightTTFTTieBreak(t *testing.T) {
	in := []Candidate{
		{DeploymentID: "unknown", InFlight: 1},
		{DeploymentID: "slow", InFlight: 1, TTFT: 400 * time.Millisecond},
		{DeploymentID: "fast", InFlight: 1, TTFT: 50 * time.Millisecond},
		{DeploymentID: "busy", InFlight: 2, TTFT: time.Millisecond},
	}
	for range 20 {
		if got := ids(rank(t, in)); !slices.Equal(got, []string{"fast", "slow", "unknown", "busy"}) {
			t.Fatalf("order = %v", got)
		}
	}
}

func TestLeastInflightRandomTies(t *testing.T) {
	in := []Candidate{{DeploymentID: "a"}, {DeploymentID: "b"}, {DeploymentID: "c"}}
	first := map[string]bool{}
	for range 200 {
		first[rank(t, in)[0].DeploymentID] = true
	}
	if len(first) != 3 {
		t.Fatalf("only %v ever ranked first", first)
	}
}

func TestLeastInflightEmpty(t *testing.T) {
	if got := rank(t, nil); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestRegistry(t *testing.T) {
	if _, err := New("nope", nil, Deps{}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
	if !slices.Contains(Names(), LeastInflight) || !slices.IsSorted(Names()) {
		t.Fatalf("names = %v", Names())
	}

	// The registry is process-global, so a repeated run (-count) finds it set.
	if !slices.Contains(Names(), "test-only") {
		Register("test-only", func(json.RawMessage, Deps) (Strategy, error) {
			return leastInflightStrategy{}, nil
		})
	}
	if _, err := New("test-only", json.RawMessage(`{}`), Deps{}); err != nil {
		t.Fatal(err)
	}
	mustPanic(t, func() { Register("test-only", func(json.RawMessage, Deps) (Strategy, error) { return nil, nil }) })
	mustPanic(t, func() { Register(LeastInflight, func(json.RawMessage, Deps) (Strategy, error) { return nil, nil }) })
	mustPanic(t, func() { Register("", func(json.RawMessage, Deps) (Strategy, error) { return nil, nil }) })
	mustPanic(t, func() { Register("nil-factory", nil) })
}

func mustPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected panic")
		}
	}()
	f()
}

// The package is a leaf so a strategy can never pull the gateway or the
// store in by accident.
func TestImportsOnlyStandardLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			first, _, _ := strings.Cut(path, "/")
			if strings.Contains(first, ".") {
				t.Errorf("%s imports non-stdlib %s", f, path)
			}
		}
	}
}
