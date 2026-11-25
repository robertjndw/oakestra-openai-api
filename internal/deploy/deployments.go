package deploy

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
	"github.com/robertjndw/oakestra-openai-api/internal/store"
)

// State is the lifecycle state of a deployment, stored as is.
type State string

// Deployment states; see the package documentation for what each step does.
const (
	StatePending    State = "pending"
	StateScheduling State = "scheduling"
	StateStarting   State = "starting"
	StateReady      State = "ready"
	StateUnhealthy  State = "unhealthy"
	StateFailed     State = "failed"
	StateDeleting   State = "deleting"
)

// serviceHashLen leaves room for a readable model-name prefix in the
// 32-character microservice name.
const serviceHashLen = 10

// Deployment is one engine of one model pinned to one node. The engine
// key, host port and service id stay internal: the key guards an open host
// port, and the rest is of no use to an admin client.
type Deployment struct {
	ID        string `json:"id"`
	ModelID   string `json:"model_id"`
	ModelName string `json:"model_name"`
	Cluster   string `json:"cluster"`
	Node      string `json:"node"`
	State     State  `json:"state"`
	// Error is the reason for failed, or the last transient problem of any
	// other state; empty when the last step went fine.
	Error     string `json:"error"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// row is a deployment with the columns the loop needs.
type row struct {
	Deployment
	nodeIP      string
	hostPort    int
	serviceName string
	serviceID   string
	engineKey   string
	stateSince  time.Time
	auto        bool
	labels      map[string]string
}

func (r row) baseURL() string {
	return "http://" + net.JoinHostPort(r.nodeIP, strconv.Itoa(r.hostPort))
}

const rowSelect = `SELECT d.id, d.model_id, m.name, d.cluster_name, d.node_name, d.state, d.error,
	d.created_at, d.updated_at, d.node_ip, d.host_port, d.service_name, d.service_id, d.engine_key,
	d.state_since, m.auto, m.labels
	FROM deployments d JOIN models m ON m.id = d.model_id`

func scanRow(s scanner) (row, error) {
	var (
		r      row
		since  int64
		auto   int
		labels string
	)
	err := s.Scan(&r.ID, &r.ModelID, &r.ModelName, &r.Cluster, &r.Node, &r.State, &r.Error,
		&r.CreatedAt, &r.UpdatedAt, &r.nodeIP, &r.hostPort, &r.serviceName, &r.serviceID, &r.engineKey,
		&since, &auto, &labels)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return row{}, err
		}
		return row{}, fmt.Errorf("deploy: read deployment: %w", err)
	}
	r.stateSince = store.FromMillis(since)
	r.auto = auto != 0
	if err := json.Unmarshal([]byte(labels), &r.labels); err != nil {
		return row{}, fmt.Errorf("deploy: deployment %s labels: %w", r.ID, err)
	}
	if r.labels == nil {
		r.labels = map[string]string{}
	}
	return r, nil
}

func (m *Manager) getRow(ctx context.Context, id string) (row, error) {
	r, err := scanRow(m.db.QueryRowContext(ctx, rowSelect+` WHERE d.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, httpx.NotFound("deployment %q not found", id)
	}
	return r, err
}

func (m *Manager) queryRows(ctx context.Context, where string, args ...any) ([]row, error) {
	rows, err := m.db.QueryContext(ctx, rowSelect+" "+where+` ORDER BY d.created_at, d.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("deploy: list deployments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deploy: list deployments: %w", err)
	}
	return out, nil
}

// ListDeployments returns every deployment, oldest first.
func (m *Manager) ListDeployments(ctx context.Context) ([]Deployment, error) {
	rows, err := m.queryRows(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]Deployment, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Deployment)
	}
	return out, nil
}

// GetDeployment returns one deployment, or a 404.
func (m *Manager) GetDeployment(ctx context.Context, id string) (Deployment, error) {
	r, err := m.getRow(ctx, id)
	return r.Deployment, err
}

// CreateDeployment pins model modelID to node in cluster (both by the names
// the node list shows) and starts deploying it. The node is checked against
// a fresh listing: it must exist, be active and advertise the model CSI
// driver, because Oakestra would otherwise accept the service and only
// report a generic scheduling failure later.
func (m *Manager) CreateDeployment(ctx context.Context, modelID, cluster, node string) (Deployment, error) {
	mod, err := m.GetModel(ctx, modelID)
	if he, ok := errors.AsType[*httpx.Error](err); ok && he.Status == http.StatusNotFound {
		return Deployment{}, httpx.Unprocessable("model %q does not exist", modelID)
	}
	if err != nil {
		return Deployment{}, err
	}
	nl, err := m.RefreshNodes(ctx)
	if err != nil {
		return Deployment{}, err
	}
	n, ok := nl.find(cluster, node)
	switch {
	case !ok && slices.Contains(nl.failedClusters, cluster):
		return Deployment{}, httpx.Unavailable("the nodes of cluster %q cannot be listed right now; try again later", cluster)
	case !ok:
		return Deployment{}, httpx.Unprocessable("node %q not found in cluster %q", node, cluster)
	case !n.Active:
		return Deployment{}, httpx.Unprocessable("node %q is not active", node)
	case !n.ModelpackCSI:
		return Deployment{}, httpx.Unprocessable("node %q does not advertise the CSI driver %s, which mounts the model; install it on the node first",
			node, m.cfg.ModelCSIDriver)
	case n.IP == "":
		return Deployment{}, httpx.Unprocessable("node %q reports no IP address, so its engine could not be reached", node)
	}

	now := m.now()
	id := store.NewID(store.PrefixDeployment)
	r := row{
		Deployment: Deployment{
			ID:        id,
			ModelID:   mod.ID,
			ModelName: mod.Name,
			Cluster:   cluster,
			Node:      node,
			State:     StatePending,
			CreatedAt: store.Millis(now),
			UpdatedAt: store.Millis(now),
		},
		nodeIP:      n.IP,
		serviceName: oakestra.HashedName(mod.Name, id, serviceHashLen),
		engineKey:   rand.Text(),
		stateSince:  now,
	}
	err = m.db.Tx(ctx, func(tx *sql.Tx) error {
		var existing State
		err := tx.QueryRowContext(ctx,
			`SELECT state FROM deployments WHERE model_id = ? AND cluster_name = ? AND node_name = ?`,
			mod.ID, cluster, node).Scan(&existing)
		if err == nil {
			return httpx.Conflict("model %q is already deployed on %s/%s (state %s)", mod.Name, cluster, node, existing)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("deploy: check duplicate: %w", err)
		}
		port, err := m.freePort(ctx, tx, n.IP)
		if err != nil {
			return err
		}
		r.hostPort = port
		_, err = tx.ExecContext(ctx,
			`INSERT INTO deployments (id, model_id, cluster_name, node_name, node_ip, host_port,
			   service_name, engine_key, state, state_since, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.ModelID, r.Cluster, r.Node, r.nodeIP, r.hostPort,
			r.serviceName, r.engineKey, r.State, store.Millis(r.stateSince), r.CreatedAt, r.UpdatedAt)
		if isUniqueViolation(err) {
			// The checks above run under the write lock, so this is only
			// a backstop.
			return httpx.Conflict("deployment conflicts with an existing one: %v", err)
		}
		if err != nil {
			return fmt.Errorf("deploy: insert deployment: %w", err)
		}
		return nil
	})
	if err != nil {
		return Deployment{}, err
	}
	m.log.Info("deploy: deployment created", "deployment", r.ID, "model", mod.Name,
		"cluster", cluster, "node", node, "host_port", r.hostPort)
	m.Kick()
	return r.Deployment, nil
}

// freePort returns the lowest port of the engine range that no deployment
// on ip uses. Ports are per IP, not per node, because several nodes can
// share one address.
func (m *Manager) freePort(ctx context.Context, tx *sql.Tx, ip string) (int, error) {
	pr := m.cfg.EnginePortRange
	rows, err := tx.QueryContext(ctx,
		`SELECT host_port FROM deployments WHERE node_ip = ? AND host_port BETWEEN ? AND ?`, ip, pr.Lo, pr.Hi)
	if err != nil {
		return 0, fmt.Errorf("deploy: used ports: %w", err)
	}
	defer func() { _ = rows.Close() }()
	used := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return 0, fmt.Errorf("deploy: used ports: %w", err)
		}
		used[p] = true
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("deploy: used ports: %w", err)
	}
	for p := pr.Lo; p <= pr.Hi; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, httpx.Unavailable("no free host port left in %s on %s; remove a deployment on that address first", pr, ip)
}

// DeleteDeployment marks a deployment for removal. The loop deletes the
// Oakestra service and then the row. Deleting a deployment that is already
// being deleted is a no-op.
func (m *Manager) DeleteDeployment(ctx context.Context, id string) error {
	now := store.Millis(m.now())
	res, err := m.db.ExecContext(ctx,
		`UPDATE deployments SET state = ?, error = '', state_since = ?, updated_at = ? WHERE id = ? AND state != ?`,
		StateDeleting, now, now, id, StateDeleting)
	if err != nil {
		return fmt.Errorf("deploy: delete deployment: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := m.getRow(ctx, id); err != nil {
			return err
		}
		return nil
	}
	m.log.Info("deploy: deployment marked for deletion", "deployment", id)
	m.publish(ctx, false)
	m.Kick()
	return nil
}

// RetryDeployment starts a failed deployment over: its Oakestra service, if
// any, is deleted first so the next attempt starts from a clean slate.
func (m *Manager) RetryDeployment(ctx context.Context, id string) (Deployment, error) {
	r, err := m.getRow(ctx, id)
	if err != nil {
		return Deployment{}, err
	}
	if r.State != StateFailed {
		return Deployment{}, httpx.Errorf(http.StatusConflict, httpx.CodeInvalidState,
			"only a failed deployment can be retried; this one is %s", r.State)
	}
	if r.serviceID != "" {
		if err := m.oak.DeleteService(ctx, r.serviceID); err != nil && !errors.Is(err, oakestra.ErrNotFound) {
			return Deployment{}, &httpx.Error{Status: http.StatusServiceUnavailable, Code: httpx.CodeUnavailable,
				Message: fmt.Sprintf("cannot remove the failed service from Oakestra: %v", err), Err: err}
		}
	}
	now := store.Millis(m.now())
	res, err := m.db.ExecContext(ctx,
		`UPDATE deployments SET state = ?, service_id = '', error = '', state_since = ?, updated_at = ?
		 WHERE id = ? AND state = ?`, StatePending, now, now, id, StateFailed)
	if err != nil {
		return Deployment{}, fmt.Errorf("deploy: retry deployment: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Deployment{}, httpx.Errorf(http.StatusConflict, httpx.CodeInvalidState,
			"deployment %q changed state while retrying", id)
	}
	m.log.Info("deploy: deployment retried", "deployment", id)
	m.Kick()
	return m.GetDeployment(ctx, id)
}
