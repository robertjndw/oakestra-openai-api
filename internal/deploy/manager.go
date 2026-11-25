package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/config"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
	"github.com/robertjndw/oakestra-openai-api/internal/store"
)

// probeTimeout bounds one engine probe request. Probes run one after the
// other inside a tick, so a black-holed node must not stall the loop.
const probeTimeout = 5 * time.Second

// Options are the dependencies of a Manager.
type Options struct {
	// Config supplies the port range, CSI driver, mount path, application
	// names, poll interval and timeouts.
	Config config.Config
	DB     *store.DB
	// Oakestra is the service-account client.
	Oakestra oakestra.Client
	// ProbeClient probes engines. Defaults to a client without proxy:
	// engines live on node IPs, which a corporate proxy cannot reach.
	ProbeClient *http.Client
	// Now is the clock for timestamps and timeouts; time.Now when nil.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Manager owns models, deployments and the deployment loop. Its methods are
// safe for concurrent use; Run must be called at most once.
type Manager struct {
	cfg   config.Config
	db    *store.DB
	oak   oakestra.Client
	probe *http.Client
	now   func() time.Time
	log   *slog.Logger
	kick  chan struct{}

	nodesMu sync.Mutex
	nodes   NodeList
	nodesAt time.Time

	// tickMu serialises ticks, so tests can drive tick directly while
	// nothing else runs one. The fields below are only touched in a tick.
	tickMu        sync.Mutex
	appID         string
	probeFailures map[string]int

	pubMu     sync.Mutex
	listeners []func([]Endpoint)
	published []Endpoint
}

// New returns a Manager. It does not contact Oakestra.
func New(opts Options) (*Manager, error) {
	if opts.DB == nil {
		return nil, errors.New("deploy: DB is required")
	}
	if opts.Oakestra == nil {
		return nil, errors.New("deploy: Oakestra client is required")
	}
	if opts.Config.PollInterval <= 0 {
		return nil, errors.New("deploy: Config.PollInterval must be positive")
	}
	m := &Manager{
		cfg:           opts.Config,
		db:            opts.DB,
		oak:           opts.Oakestra,
		probe:         opts.ProbeClient,
		now:           opts.Now,
		log:           opts.Logger,
		kick:          make(chan struct{}, 1),
		probeFailures: make(map[string]int),
	}
	if m.probe == nil {
		m.probe = &http.Client{Timeout: probeTimeout, Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: probeTimeout}).DialContext,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		}}
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	return m, nil
}

// Run publishes the routable endpoints once, then runs the deployment loop
// every PollInterval and whenever Kick is called, until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	m.publish(ctx, true)
	t := time.NewTicker(m.cfg.PollInterval)
	defer t.Stop()
	for {
		m.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.kick:
		}
	}
}

// Kick makes the loop run a tick soon. It never blocks; kicks that arrive
// while one is pending collapse into one.
func (m *Manager) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// ReportFailure is called by the gateway when it could not connect to the
// engine of a deployment. It only makes the loop probe soon: one refused
// connection is not enough to take an engine out of rotation, three failed
// probes are.
func (m *Manager) ReportFailure(deploymentID string) {
	m.log.Debug("deploy: gateway reported an unreachable engine", "deployment", deploymentID)
	m.Kick()
}

// Endpoint is one routable (ready) deployment as the gateway needs it.
type Endpoint struct {
	DeploymentID string
	ModelID      string
	ModelName    string
	Auto         bool
	Labels       map[string]string
	Cluster      string
	Node         string
	// BaseURL is http://<node ip>:<host port>, without trailing slash.
	BaseURL string
	// EngineKey must be sent as the bearer token to the engine.
	EngineKey string
}

// OnChange registers fn to receive the full set of routable endpoints
// whenever it changes, and once when Run starts. Register before Run. fn
// runs synchronously on the goroutine that caused the change and must not
// call back into the Manager.
func (m *Manager) OnChange(fn func([]Endpoint)) {
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	m.listeners = append(m.listeners, fn)
}

// publish tells the listeners about the routable endpoints if they changed
// since the last call, or always when force is set.
func (m *Manager) publish(ctx context.Context, force bool) {
	// Read under the lock: two publishers racing could otherwise deliver
	// the older snapshot last.
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	eps, err := m.endpoints(ctx)
	if err != nil {
		m.log.Error("deploy: read endpoints", "err", err)
		return
	}
	if !force && m.published != nil && reflect.DeepEqual(eps, m.published) {
		return
	}
	m.published = eps
	for _, fn := range m.listeners {
		fn(eps)
	}
}

func (m *Manager) endpoints(ctx context.Context) ([]Endpoint, error) {
	rows, err := m.db.QueryContext(ctx, rowSelect+` WHERE d.state = ? ORDER BY d.id`, StateReady)
	if err != nil {
		return nil, fmt.Errorf("deploy: list endpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Endpoint{}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, Endpoint{
			DeploymentID: r.ID,
			ModelID:      r.ModelID,
			ModelName:    r.ModelName,
			Auto:         r.auto,
			Labels:       r.labels,
			Cluster:      r.Cluster,
			Node:         r.Node,
			BaseURL:      r.baseURL(),
			EngineKey:    r.engineKey,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deploy: list endpoints: %w", err)
	}
	return out, nil
}

// Counts summarises models and deployments for the overview page.
type Counts struct {
	Models      int `json:"models"`
	Deployments int `json:"deployments"`
	Ready       int `json:"ready"`
}

// Counts returns the number of models, deployments and ready deployments.
func (m *Manager) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	err := m.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM models),
		(SELECT COUNT(*) FROM deployments),
		(SELECT COUNT(*) FROM deployments WHERE state = ?)`, StateReady).
		Scan(&c.Models, &c.Deployments, &c.Ready)
	if err != nil {
		return Counts{}, fmt.Errorf("deploy: counts: %w", err)
	}
	return c, nil
}

// CheckOakestra reports whether Oakestra is reachable with the service
// account. It lists the account's applications: the cluster list would be
// cheaper, but it is public and so would not catch wrong credentials.
func (m *Manager) CheckOakestra(ctx context.Context) error {
	_, err := m.oak.GetApplications(ctx)
	return err
}
