// Package api serves the admin REST API under /admin/v1, which the addon UI
// reaches through its nginx. Every route needs an authenticated dashboard
// user; every mutation needs an admin.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/auth"
	"github.com/robertjndw/oakestra-openai-api/internal/config"
	"github.com/robertjndw/oakestra-openai-api/internal/deploy"
	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
	"github.com/robertjndw/oakestra-openai-api/internal/scheduler"
	"github.com/robertjndw/oakestra-openai-api/internal/store"
)

// Prefix is where the admin API is mounted.
const Prefix = "/admin/v1"

// oakestraCheckTimeout keeps the overview page responsive while Oakestra
// is down: the client would otherwise retry for its full request timeout.
const oakestraCheckTimeout = 3 * time.Second

// StrategySetter receives the routing strategy chosen in the settings.
type StrategySetter interface {
	SetStrategy(scheduler.Strategy)
}

// Options are the dependencies of the admin API.
type Options struct {
	DB     *store.DB
	Deploy *deploy.Manager
	Keys   *auth.Keys
	// Gateway gets the new strategy when the settings change.
	Gateway StrategySetter
	// SchedulerDeps is handed to every strategy built from the settings.
	SchedulerDeps scheduler.Deps
	// Scheduler is the setting currently in effect, as returned by
	// LoadScheduler at startup.
	Scheduler SchedulerSetting

	AuthMode     config.AdminAuthMode
	Introspector *auth.Introspector

	PublicURL string
	Version   string
	Logger    *slog.Logger
}

// API is the admin API.
type API struct {
	opts Options
	log  *slog.Logger

	// schedMu makes a settings change (validate, persist, swap) atomic, so
	// two concurrent PUTs cannot leave the gateway on one strategy and the
	// database on the other.
	schedMu sync.Mutex
	sched   SchedulerSetting
}

// New returns the admin API.
func New(o Options) *API {
	a := &API{opts: o, log: o.Logger, sched: o.Scheduler}
	if a.log == nil {
		a.log = slog.Default()
	}
	return a
}

// Handler serves every route below Prefix.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	read := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, h)
	}
	write := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, auth.RequireAdmin(h))
	}

	read("GET "+Prefix+"/me", a.me)
	read("GET "+Prefix+"/overview", a.overview)

	read("GET "+Prefix+"/models", a.listModels)
	write("POST "+Prefix+"/models", a.createModel)
	read("GET "+Prefix+"/models/{id}", a.getModel)
	write("PATCH "+Prefix+"/models/{id}", a.updateModel)
	write("DELETE "+Prefix+"/models/{id}", a.deleteModel)

	read("GET "+Prefix+"/nodes", a.nodes)

	read("GET "+Prefix+"/deployments", a.listDeployments)
	write("POST "+Prefix+"/deployments", a.createDeployment)
	write("DELETE "+Prefix+"/deployments/{id}", a.deleteDeployment)
	write("POST "+Prefix+"/deployments/{id}/retry", a.retryDeployment)

	read("GET "+Prefix+"/keys", a.listKeys)
	write("POST "+Prefix+"/keys", a.createKey)
	write("DELETE "+Prefix+"/keys/{id}", a.deleteKey)

	read("GET "+Prefix+"/settings", a.getSettings)
	write("PUT "+Prefix+"/settings", a.putSettings)

	// The catch-all also takes requests whose path exists with another
	// method; a JSON 404 is more useful to the UI than net/http's plain 405.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, httpx.NotFound("no route %s %s", r.Method, r.URL.Path))
	})
	return auth.Require(a.opts.AuthMode, a.opts.Introspector)(mux)
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	if u.Roles == nil {
		u.Roles = []string{}
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

type oakestraStatus struct {
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

type overview struct {
	PublicURL   string         `json:"public_url"`
	Version     string         `json:"version"`
	Oakestra    oakestraStatus `json:"oakestra"`
	Models      int            `json:"models"`
	Deployments struct {
		Total int `json:"total"`
		Ready int `json:"ready"`
	} `json:"deployments"`
	Keys      int    `json:"keys"`
	Scheduler string `json:"scheduler"`
}

func (a *API) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	counts, err := a.opts.Deploy.Counts(ctx)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	keys, err := a.opts.Keys.Count(ctx)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := overview{
		PublicURL: a.opts.PublicURL,
		Version:   a.opts.Version,
		Models:    counts.Models,
		Keys:      keys,
		Scheduler: a.currentScheduler().Strategy,
	}
	out.Deployments.Total = counts.Deployments
	out.Deployments.Ready = counts.Ready

	checkCtx, cancel := context.WithTimeout(ctx, oakestraCheckTimeout)
	defer cancel()
	if err := a.opts.Deploy.CheckOakestra(checkCtx); err != nil {
		out.Oakestra.Error = err.Error()
	} else {
		out.Oakestra.Reachable = true
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (a *API) listModels(w http.ResponseWriter, r *http.Request) {
	models, err := a.opts.Deploy.ListModels(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (a *API) createModel(w http.ResponseWriter, r *http.Request) {
	var in deploy.ModelInput
	if err := httpx.ReadJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	m, err := a.opts.Deploy.CreateModel(r.Context(), in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, m)
}

func (a *API) getModel(w http.ResponseWriter, r *http.Request) {
	m, err := a.opts.Deploy.GetModel(r.Context(), r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

func (a *API) updateModel(w http.ResponseWriter, r *http.Request) {
	var p deploy.ModelPatch
	if err := httpx.ReadJSON(r, &p); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	m, err := a.opts.Deploy.UpdateModel(r.Context(), r.PathValue("id"), p)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

func (a *API) deleteModel(w http.ResponseWriter, r *http.Request) {
	if err := a.opts.Deploy.DeleteModel(r.Context(), r.PathValue("id")); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (a *API) nodes(w http.ResponseWriter, r *http.Request) {
	nl, err := a.opts.Deploy.Nodes(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, nl)
}

func (a *API) listDeployments(w http.ResponseWriter, r *http.Request) {
	ds, err := a.opts.Deploy.ListDeployments(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deployments": ds})
}

func (a *API) createDeployment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ModelID string `json:"model_id"`
		Cluster string `json:"cluster"`
		Node    string `json:"node"`
	}
	if err := httpx.ReadJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if in.ModelID == "" || in.Cluster == "" || in.Node == "" {
		httpx.WriteError(w, r, httpx.Unprocessable("model_id, cluster and node are required"))
		return
	}
	d, err := a.opts.Deploy.CreateDeployment(r.Context(), in.ModelID, in.Cluster, in.Node)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, d)
}

// deleteDeployment answers 202: the loop removes the Oakestra service and
// then the row, which the client sees by polling the list.
func (a *API) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	if err := a.opts.Deploy.DeleteDeployment(r.Context(), r.PathValue("id")); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *API) retryDeployment(w http.ResponseWriter, r *http.Request) {
	d, err := a.opts.Deploy.RetryDeployment(r.Context(), r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, d)
}

func (a *API) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := a.opts.Keys.List(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (a *API) createKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := httpx.ReadJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	key, secret, err := a.opts.Keys.Create(r.Context(), in.Name)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"key": key, "secret": secret})
}

func (a *API) deleteKey(w http.ResponseWriter, r *http.Request) {
	if err := a.opts.Keys.Revoke(r.Context(), r.PathValue("id")); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.NoContent(w)
}
