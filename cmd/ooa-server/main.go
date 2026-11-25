// Command ooa-server is the addon backend: the OpenAI-compatible gateway on
// the public listener, the admin API for the addon UI on a second listener,
// and the loop that drives deployments through Oakestra.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/api"
	"github.com/robertjndw/oakestra-openai-api/internal/auth"
	"github.com/robertjndw/oakestra-openai-api/internal/config"
	"github.com/robertjndw/oakestra-openai-api/internal/deploy"
	"github.com/robertjndw/oakestra-openai-api/internal/gateway"
	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
	"github.com/robertjndw/oakestra-openai-api/internal/scheduler"
	"github.com/robertjndw/oakestra-openai-api/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("ooa-server failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := httpx.NewLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	logger.Info("starting ooa-server", "version", version, "listen", cfg.Listen,
		"admin_listen", cfg.AdminListen, "oakestra", cfg.Oakestra.API, "admin_auth", cfg.AdminAuth)
	for _, w := range cfg.Warnings() {
		logger.Warn(w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	oakOpts := oakestra.OptionsFromConfig(cfg)
	oakOpts.Logger = logger
	oak, err := oakestra.New(oakOpts)
	if err != nil {
		return err
	}
	mgr, err := deploy.New(deploy.Options{Config: cfg, DB: db, Oakestra: oak, Logger: logger})
	if err != nil {
		return err
	}
	keys := auth.NewKeys(db)

	gw := gateway.New(gateway.Options{
		Auth:         keys,
		MaxAttempts:  cfg.MaxAttempts,
		MaxBodyBytes: int64(cfg.MaxBodyBytes),
		DebugHeaders: cfg.DebugHeaders,
		Logger:       logger,
		KnownModel:   knownModel(ctx, db),
	})
	deps := scheduler.Deps{Complete: gw.Complete, Logger: logger}
	strategy, setting := api.LoadScheduler(ctx, db, cfg.Scheduler, deps, logger)
	gw.SetStrategy(strategy)
	logger.Info("scheduler strategy", "strategy", strategy.Name())

	mgr.OnChange(func(eps []deploy.Endpoint) { gw.SetEndpoints(toGateway(eps)) })
	gw.OnUpstreamFailure(mgr.ReportFailure)

	var in *auth.Introspector
	if cfg.AdminAuth == config.AdminAuthIntrospect {
		in = auth.NewIntrospector(cfg.AuthURL, nil)
	}
	admin := api.New(api.Options{
		DB: db, Deploy: mgr, Keys: keys, Gateway: gw,
		SchedulerDeps: deps, Scheduler: setting,
		AuthMode: cfg.AdminAuth, Introspector: in,
		PublicURL: cfg.PublicURL, Version: version, Logger: logger,
	})

	go checkOakestra(ctx, mgr, logger)

	// The loop and both listeners share one context: whichever stops first
	// (a signal, a listener that cannot bind) stops the others, and each
	// finishes its bounded shutdown before the database closes.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 3)
	go func() {
		mgr.Run(ctx)
		errs <- nil
	}()
	go func() { errs <- httpx.Serve(ctx, cfg.Listen, publicHandler(gw, db, logger)) }()
	go func() { errs <- httpx.Serve(ctx, cfg.AdminListen, adminHandler(admin, logger)) }()

	var all []error
	for range 3 {
		if err := <-errs; err != nil {
			all = append(all, err)
		}
		cancel()
	}
	logger.Info("ooa-server stopped")
	return errors.Join(all...)
}

// publicHandler serves /v1 and the health probes. Nothing else is routed
// here, so the admin API cannot be reached on the public port.
func publicHandler(gw *gateway.Gateway, db *store.DB, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/", gw.Handler())
	mux.Handle("GET /healthz", httpx.Healthz(version))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			httpx.WriteError(w, r, &httpx.Error{Status: http.StatusServiceUnavailable,
				Code: httpx.CodeUnavailable, Message: "database unavailable", Err: err})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("/", notFound)
	return httpx.Chain(mux, httpx.RequestIDs, httpx.Logging(logger), httpx.Recover)
}

func adminHandler(admin *api.API, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(api.Prefix+"/", admin.Handler())
	mux.Handle("GET /healthz", httpx.Healthz(version))
	mux.HandleFunc("/", notFound)
	return httpx.Chain(mux, httpx.RequestIDs, httpx.Logging(logger), httpx.Recover)
}

func notFound(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, httpx.NotFound("no route %s %s", r.Method, r.URL.Path))
}

// knownModel lets the gateway tell an unknown model (404) from one with no
// ready deployment (503). The gateway asks only when nothing is routable
// for the name, so a primary-key lookup per such request is cheap enough
// and never stale.
func knownModel(ctx context.Context, db *store.DB) func(string) bool {
	return func(name string) bool {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var one int
		err := db.QueryRowContext(ctx, `SELECT 1 FROM models WHERE name = ?`, name).Scan(&one)
		return err == nil
	}
}

func toGateway(eps []deploy.Endpoint) []gateway.Endpoint {
	out := make([]gateway.Endpoint, len(eps))
	for i, e := range eps {
		out[i] = gateway.Endpoint{
			DeploymentID: e.DeploymentID,
			ModelID:      e.ModelID,
			ModelName:    e.ModelName,
			Auto:         e.Auto,
			Labels:       e.Labels,
			Cluster:      e.Cluster,
			Node:         e.Node,
			BaseURL:      e.BaseURL,
			EngineKey:    e.EngineKey,
		}
	}
	return out
}

// checkOakestra only logs: the server must come up without Oakestra, and
// the deployment loop keeps retrying on its own.
func checkOakestra(ctx context.Context, mgr *deploy.Manager, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := mgr.CheckOakestra(ctx); err != nil {
		logger.Warn("Oakestra is not reachable yet; deployments wait until it is", "err", err)
		return
	}
	logger.Info("Oakestra reachable with the service account")
}
