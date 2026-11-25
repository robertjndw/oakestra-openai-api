package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
	"github.com/robertjndw/oakestra-openai-api/internal/scheduler"
	"github.com/robertjndw/oakestra-openai-api/internal/store"
)

// SchedulerSetting is the value of store.SettingScheduler.
type SchedulerSetting struct {
	Strategy string `json:"strategy"`
	// Config is the strategy's own configuration, passed to its factory
	// unchanged. null when the strategy needs none.
	Config json.RawMessage `json:"config"`
}

// LoadScheduler builds the strategy to start with: the persisted setting,
// else the configured fallback (OOA_SCHEDULER), else least-inflight. A bad
// setting is logged instead of returned, because routing with the default
// beats refusing to serve /v1 at all.
func LoadScheduler(ctx context.Context, db *store.DB, fallback string, deps scheduler.Deps, log *slog.Logger) (scheduler.Strategy, SchedulerSetting) {
	var candidates []SchedulerSetting
	saved, ok, err := store.GetSettingAs[SchedulerSetting](ctx, db, store.SettingScheduler)
	switch {
	case err != nil:
		log.Error("api: cannot read the scheduler setting; using the configured one", "err", err)
	case ok:
		candidates = append(candidates, saved)
	}
	candidates = append(candidates,
		SchedulerSetting{Strategy: fallback},
		SchedulerSetting{Strategy: scheduler.LeastInflight})

	for i, c := range candidates {
		s, err := scheduler.New(c.Strategy, c.Config, deps)
		if err == nil {
			return s, normalize(c)
		}
		if i < len(candidates)-1 {
			log.Error("api: unusable scheduler strategy, falling back", "strategy", c.Strategy, "err", err)
		}
	}
	// least-inflight is registered by the scheduler package itself and
	// ignores its config, so the loop above always returns.
	panic("api: least-inflight strategy is not registered")
}

func normalize(s SchedulerSetting) SchedulerSetting {
	if len(bytes.TrimSpace(s.Config)) == 0 {
		s.Config = json.RawMessage("null")
	}
	return s
}

func (a *API) currentScheduler() SchedulerSetting {
	a.schedMu.Lock()
	defer a.schedMu.Unlock()
	return a.sched
}

type settings struct {
	Scheduler  SchedulerSetting `json:"scheduler"`
	Strategies []string         `json:"strategies"`
}

func (a *API) settings() settings {
	return settings{Scheduler: a.currentScheduler(), Strategies: scheduler.Names()}
}

func (a *API) getSettings(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, a.settings())
}

// putSettings builds the strategy before persisting it, so an unknown name
// or a config its factory rejects leaves both the database and the gateway
// on the previous strategy.
func (a *API) putSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scheduler SchedulerSetting `json:"scheduler"`
	}
	if err := httpx.ReadJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	next := normalize(in.Scheduler)

	a.schedMu.Lock()
	s, err := scheduler.New(next.Strategy, next.Config, a.opts.SchedulerDeps)
	if err != nil {
		a.schedMu.Unlock()
		httpx.WriteError(w, r, httpx.Unprocessable("invalid scheduler: %v", err))
		return
	}
	if err := store.PutSetting(r.Context(), a.opts.DB, store.SettingScheduler, next); err != nil {
		a.schedMu.Unlock()
		httpx.WriteError(w, r, err)
		return
	}
	a.opts.Gateway.SetStrategy(s)
	a.sched = next
	a.schedMu.Unlock()

	a.log.Info("api: scheduler strategy changed", "strategy", next.Strategy)
	httpx.WriteJSON(w, http.StatusOK, a.settings())
}
