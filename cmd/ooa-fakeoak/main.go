package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra/oakestratest"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("ooa-fakeoak failed", "err", err)
		os.Exit(1)
	}
}

// envPrefix turns flag -node-ip into OOA_FAKEOAK_NODE_IP.
const envPrefix = "OOA_FAKEOAK_"

type flags struct {
	listen       string
	nodes        int
	clusters     int
	nodeMemMB    int
	nodeUsedMB   int
	arch         string
	gpus         int
	vramMB       int
	nodeIP       string
	clusterIP    string
	noModelpack  []string
	user         string
	password     string
	spawnEngines bool
	engineBind   string
	publicURL    string
}

// parseFlags reads args with defaults taken from the environment through
// lookup (os.LookupEnv in production), so compose can configure the fake
// without a command line.
func parseFlags(args []string, lookup func(string) (string, bool)) (flags, error) {
	var f flags
	var noModelpack string
	fs := flag.NewFlagSet("ooa-fakeoak", flag.ContinueOnError)
	fs.StringVar(&f.listen, "listen", ":10000", "listen address for the fake system manager and abstractors")
	fs.IntVar(&f.nodes, "nodes", 3, "number of simulated worker nodes (node1, node2, ...), spread round-robin over the clusters")
	fs.IntVar(&f.clusters, "clusters", 1, "number of clusters (cluster1, cluster2, ...)")
	fs.IntVar(&f.nodeMemMB, "node-mem-mb", 131072, "total memory per node in MiB")
	fs.IntVar(&f.nodeUsedMB, "node-used-mb", 4096, "memory in use by other software per node in MiB")
	fs.StringVar(&f.arch, "arch", "amd64", "node architecture (GOARCH)")
	fs.IntVar(&f.gpus, "gpus", 1, "GPUs per node; 0 simulates CPU-only nodes")
	fs.IntVar(&f.vramMB, "vram-mb", 24576, "VRAM per node in MiB")
	fs.StringVar(&f.nodeIP, "node-ip", "127.0.0.1", "IP or hostname every node reports; engine host ports are reached there")
	fs.StringVar(&f.clusterIP, "cluster-ip", "127.0.0.254", "IP every cluster reports; nothing listens there, use the abstractor overrides")
	fs.StringVar(&noModelpack, "no-modelpack", "", "comma-separated node names that lack the modelpack CSI driver")
	fs.StringVar(&f.user, "user", "", "accept only this user (any credentials when empty)")
	fs.StringVar(&f.password, "password", "", "password for -user")
	fs.BoolVar(&f.spawnEngines, "spawn-engines", true, "start a fake OpenAI engine on <engine-bind>:<host port> for every deployed instance")
	fs.StringVar(&f.engineBind, "engine-bind", "127.0.0.1", "address spawned engines listen on; 0.0.0.0 in containers")
	fs.StringVar(&f.publicURL, "public-url", "", "base URL the server reaches this fake at, for the printed settings")

	var envErrs []error
	fs.VisitAll(func(fl *flag.Flag) {
		key := envPrefix + strings.ToUpper(strings.ReplaceAll(fl.Name, "-", "_"))
		if v, ok := lookup(key); ok {
			if err := fs.Set(fl.Name, v); err != nil {
				envErrs = append(envErrs, fmt.Errorf("%s: %w", key, err))
			}
		}
	})
	if err := errors.Join(envErrs...); err != nil {
		return flags{}, err
	}
	if err := fs.Parse(args); err != nil {
		return flags{}, err
	}
	for name := range strings.SplitSeq(noModelpack, ",") {
		if name = strings.TrimSpace(name); name != "" {
			f.noModelpack = append(f.noModelpack, name)
		}
	}
	switch {
	case f.nodes < 0:
		return flags{}, errors.New("-nodes must not be negative")
	case f.clusters < 1:
		return flags{}, errors.New("-clusters must be at least 1")
	case f.nodeUsedMB >= f.nodeMemMB:
		return flags{}, errors.New("-node-used-mb must be below -node-mem-mb")
	case (f.user == "") != (f.password == ""):
		return flags{}, errors.New("-user and -password go together")
	case f.nodeIP == "":
		return flags{}, errors.New("-node-ip is empty")
	}
	// A typo here would silently give every node the driver, which defeats
	// the point of the flag.
	for _, name := range f.noModelpack {
		if !slices.Contains(nodeNames(f.nodes), name) {
			return flags{}, fmt.Errorf("-no-modelpack: %q is not one of the simulated nodes node1..node%d", name, f.nodes)
		}
	}
	return f, nil
}

func run(args []string) error {
	f, err := parseFlags(args, os.LookupEnv)
	if err != nil {
		return err
	}
	logger := httpx.NewLogger(slog.LevelInfo)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	h, closeEngines, err := newHandler(f, logger)
	if err != nil {
		return err
	}
	defer closeEngines()

	logger.Info("starting ooa-fakeoak", "listen", f.listen, "clusters", f.clusters, "nodes", f.nodes,
		"node_ip", f.nodeIP, "no_modelpack", f.noModelpack, "spawn_engines", f.spawnEngines, "engine_bind", f.engineBind)
	logSettings(logger, f)
	return httpx.Serve(ctx, f.listen, h)
}

// newHandler builds the seeded fake with its admin API. The returned func
// stops every spawned engine.
func newHandler(f flags, logger *slog.Logger) (http.Handler, func(), error) {
	opts := oakestratest.Options{Logger: logger}
	if f.user != "" {
		opts.Users = map[string]string{f.user: f.password}
	}
	closeEngines := func() {}
	if f.spawnEngines {
		eng := newEngines(f.engineBind, logger)
		opts.OnDeploy = eng.start
		opts.OnUndeploy = eng.stop
		closeEngines = eng.closeAll
	}
	fake := oakestratest.New(opts)
	if err := seed(fake, f); err != nil {
		closeEngines()
		return nil, nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpx.Healthz("ooa-fakeoak"))
	(&admin{fake: fake, defaults: f}).routes(mux)
	mux.Handle("/", fake)
	return httpx.Chain(mux, httpx.RequestIDs, httpx.Logging(logger), httpx.Recover), closeEngines, nil
}

func clusterName(i int) string { return fmt.Sprintf("cluster%d", i+1) }

func nodeNames(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf("node%d", i+1))
	}
	return out
}

func seed(fake *oakestratest.Fake, f flags) error {
	for i := range f.clusters {
		fake.AddCluster(clusterName(i), f.clusterIP)
	}
	for i, name := range nodeNames(f.nodes) {
		spec := nodeSpec(f, name)
		if slices.Contains(f.noModelpack, name) {
			spec.CSIDrivers = []string{oakestra.HostpathCSIDriver}
		}
		if _, err := fake.AddNode(clusterName(i%f.clusters), spec); err != nil {
			return err
		}
	}
	return nil
}

func nodeSpec(f flags, name string) oakestratest.NodeSpec {
	return oakestratest.NodeSpec{
		Name:     name,
		IP:       f.nodeIP,
		Arch:     f.arch,
		MemoryMB: f.nodeMemMB,
		UsedMB:   f.nodeUsedMB,
		GPUs:     f.gpus,
		NoGPU:    f.gpus == 0,
		VRAMMB:   f.vramMB,
	}
}

// baseURL is how the server reaches this fake: -public-url, or the listen
// address with a wildcard host replaced by localhost.
func baseURL(f flags) string {
	if f.publicURL != "" {
		return strings.TrimRight(f.publicURL, "/")
	}
	host, port, err := net.SplitHostPort(f.listen)
	if err != nil {
		return "http://localhost:10000"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// abstractorOverrides is the OOA_CLUSTER_ABSTRACTOR_OVERRIDES value that
// reaches every cluster's abstractor on this fake.
func abstractorOverrides(f flags) string {
	base := baseURL(f)
	pairs := make([]string, 0, f.clusters)
	for i := range f.clusters {
		pairs = append(pairs, clusterName(i)+"="+base+oakestratest.AbstractorPath(clusterName(i)))
	}
	return strings.Join(pairs, ",")
}

func logSettings(logger *slog.Logger, f flags) {
	logger.Info("server settings for this fake",
		"OOA_OAKESTRA_API", baseURL(f),
		"OOA_CLUSTER_ABSTRACTOR_OVERRIDES", abstractorOverrides(f))
}
