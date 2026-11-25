package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
	"github.com/robertjndw/oakestra-openai-api/internal/store"
)

// DefaultContainerPort is the port vLLM listens on inside the container.
const DefaultContainerPort = 8000

// AutoModel is the model name the gateway resolves by strategy, so no
// model may be called that.
const AutoModel = "auto"

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Model is something that can be deployed. Name is the public id clients
// send as "model".
type Model struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Reference   string   `json:"reference"`
	EngineImage string   `json:"engine_image"`
	EngineArgs  []string `json:"engine_args"`
	// Command is nil for the default vLLM argv; see EngineArgv.
	Command       []string          `json:"command"`
	ContainerPort int               `json:"container_port"`
	MemoryMB      int               `json:"memory_mb"`
	VGPUs         int               `json:"vgpus"`
	Auto          bool              `json:"auto"`
	Labels        map[string]string `json:"labels"`
	CreatedAt     int64             `json:"created_at"`
	UpdatedAt     int64             `json:"updated_at"`
}

// ModelInput creates a model. Empty engine_image means the configured
// default, container_port 0 means 8000 and auto defaults to true.
type ModelInput struct {
	Name          string            `json:"name"`
	Reference     string            `json:"reference"`
	EngineImage   string            `json:"engine_image,omitempty"`
	EngineArgs    []string          `json:"engine_args,omitempty"`
	Command       []string          `json:"command,omitempty"`
	ContainerPort int               `json:"container_port,omitempty"`
	MemoryMB      int               `json:"memory_mb"`
	VGPUs         int               `json:"vgpus,omitempty"`
	Auto          *bool             `json:"auto,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
}

// ModelPatch changes the non-nil fields of a model. JSON null cannot be
// told apart from an absent field, so an empty command list is what resets
// a model to the default vLLM argv, and an empty engine_image resets it to
// the configured default image.
//
// While a model has deployments only Auto and Labels may change: every
// other field is baked into running services.
type ModelPatch struct {
	Name          *string            `json:"name,omitempty"`
	Reference     *string            `json:"reference,omitempty"`
	EngineImage   *string            `json:"engine_image,omitempty"`
	EngineArgs    *[]string          `json:"engine_args,omitempty"`
	Command       *[]string          `json:"command,omitempty"`
	ContainerPort *int               `json:"container_port,omitempty"`
	MemoryMB      *int               `json:"memory_mb,omitempty"`
	VGPUs         *int               `json:"vgpus,omitempty"`
	Auto          *bool              `json:"auto,omitempty"`
	Labels        *map[string]string `json:"labels,omitempty"`
}

// CreateModel validates in and stores it as a new model.
func (m *Manager) CreateModel(ctx context.Context, in ModelInput) (Model, error) {
	now := store.Millis(m.now())
	mod := Model{
		ID:            store.NewID(store.PrefixModel),
		Name:          in.Name,
		Reference:     in.Reference,
		EngineImage:   in.EngineImage,
		EngineArgs:    slices.Clone(in.EngineArgs),
		Command:       slices.Clone(in.Command),
		ContainerPort: in.ContainerPort,
		MemoryMB:      in.MemoryMB,
		VGPUs:         in.VGPUs,
		Auto:          in.Auto == nil || *in.Auto,
		Labels:        maps.Clone(in.Labels),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	m.normalize(&mod)
	if err := validateModel(mod); err != nil {
		return Model{}, err
	}
	cols, err := modelColumns(mod)
	if err != nil {
		return Model{}, err
	}
	_, err = m.db.ExecContext(ctx,
		`INSERT INTO models (id, name, reference, engine_image, engine_args, command, container_port,
		   memory_mb, vgpus, auto, labels, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, cols...)
	if isUniqueViolation(err) {
		return Model{}, httpx.Conflict("a model named %q exists already", mod.Name)
	}
	if err != nil {
		return Model{}, fmt.Errorf("deploy: create model: %w", err)
	}
	return mod, nil
}

// GetModel returns the model with the given id, or a 404.
func (m *Manager) GetModel(ctx context.Context, id string) (Model, error) {
	return getModel(ctx, m.db, `WHERE id = ?`, id)
}

// GetModelByName returns the model with the given public name, or a 404.
func (m *Manager) GetModelByName(ctx context.Context, name string) (Model, error) {
	return getModel(ctx, m.db, `WHERE name = ?`, name)
}

// ListModels returns every model ordered by name.
func (m *Manager) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := m.db.QueryContext(ctx, modelSelect+` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("deploy: list models: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Model{}
	for rows.Next() {
		mod, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, mod)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deploy: list models: %w", err)
	}
	return out, nil
}

// UpdateModel applies p. A patch that only repeats current values of the
// locked fields is accepted for a deployed model, so a form that always
// sends every field still works.
func (m *Manager) UpdateModel(ctx context.Context, id string, p ModelPatch) (Model, error) {
	var out Model
	err := m.db.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := getModel(ctx, tx, `WHERE id = ?`, id)
		if err != nil {
			return err
		}
		next := applyPatch(cur, p)
		m.normalize(&next)
		if err := validateModel(next); err != nil {
			return err
		}
		if !sameEngine(cur, next) {
			n, err := countDeployments(ctx, tx, id)
			if err != nil {
				return err
			}
			if n > 0 {
				return httpx.Conflict("model %q has %d deployment(s); only auto and labels can change until they are removed", cur.Name, n)
			}
		}
		next.UpdatedAt = store.Millis(m.now())
		cols, err := modelColumns(next)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE models SET name = ?2, reference = ?3, engine_image = ?4, engine_args = ?5, command = ?6,
			   container_port = ?7, memory_mb = ?8, vgpus = ?9, auto = ?10, labels = ?11, updated_at = ?13
			 WHERE id = ?1`, cols...)
		if isUniqueViolation(err) {
			return httpx.Conflict("a model named %q exists already", next.Name)
		}
		if err != nil {
			return fmt.Errorf("deploy: update model: %w", err)
		}
		out = next
		return nil
	})
	if err != nil {
		return Model{}, err
	}
	// Auto and labels are part of every endpoint of this model.
	m.publish(ctx, false)
	return out, nil
}

// DeleteModel removes a model that has no deployments.
func (m *Manager) DeleteModel(ctx context.Context, id string) error {
	return m.db.Tx(ctx, func(tx *sql.Tx) error {
		n, err := countDeployments(ctx, tx, id)
		if err != nil {
			return err
		}
		if n > 0 {
			return httpx.Conflict("model has %d deployment(s); remove them first", n)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM models WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("deploy: delete model: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return httpx.NotFound("model %q not found", id)
		}
		return nil
	})
}

func (m *Manager) normalize(mod *Model) {
	mod.Name = strings.TrimSpace(mod.Name)
	mod.Reference = strings.TrimSpace(mod.Reference)
	mod.EngineImage = strings.TrimSpace(mod.EngineImage)
	if mod.EngineImage == "" {
		mod.EngineImage = m.cfg.DefaultEngineImage
	}
	if mod.EngineArgs == nil {
		mod.EngineArgs = []string{}
	}
	if len(mod.Command) == 0 {
		mod.Command = nil
	}
	if mod.ContainerPort == 0 {
		mod.ContainerPort = DefaultContainerPort
	}
	if mod.Labels == nil {
		mod.Labels = map[string]string{}
	}
}

func validateModel(mod Model) error {
	problems := map[string]string{}
	switch {
	case !modelNamePattern.MatchString(mod.Name):
		problems["name"] = "must be 1 to 64 characters of A-Z, a-z, 0-9, '.', '_' or '-'"
	case strings.EqualFold(mod.Name, AutoModel):
		problems["name"] = `"auto" is reserved for automatic model selection`
	}
	if mod.Reference == "" {
		problems["reference"] = "is required"
	}
	if mod.ContainerPort < 1 || mod.ContainerPort > 65535 {
		problems["container_port"] = "must be a port between 1 and 65535"
	}
	if mod.MemoryMB <= 0 {
		problems["memory_mb"] = "must be positive"
	}
	if mod.VGPUs < 0 {
		problems["vgpus"] = "must not be negative"
	}
	if len(mod.Command) > 0 && strings.TrimSpace(mod.Command[0]) == "" {
		problems["command"] = "must start with the executable"
	}
	if _, ok := mod.Labels[""]; ok {
		problems["labels"] = "keys must not be empty"
	}
	if len(problems) == 0 {
		return nil
	}
	parts := make([]string, 0, len(problems))
	for _, k := range slices.Sorted(maps.Keys(problems)) {
		parts = append(parts, k+" "+problems[k])
	}
	e := httpx.Unprocessable("invalid model: %s", strings.Join(parts, "; "))
	e.Details = problems
	return e
}

func applyPatch(cur Model, p ModelPatch) Model {
	next := cur
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&next.Name, p.Name)
	set(&next.Reference, p.Reference)
	set(&next.EngineImage, p.EngineImage)
	if p.EngineArgs != nil {
		next.EngineArgs = slices.Clone(*p.EngineArgs)
	}
	if p.Command != nil {
		next.Command = slices.Clone(*p.Command)
	}
	for _, f := range []struct {
		dst *int
		src *int
	}{{&next.ContainerPort, p.ContainerPort}, {&next.MemoryMB, p.MemoryMB}, {&next.VGPUs, p.VGPUs}} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
	if p.Auto != nil {
		next.Auto = *p.Auto
	}
	if p.Labels != nil {
		next.Labels = maps.Clone(*p.Labels)
	}
	return next
}

// sameEngine reports whether a and b render the same service. Command is
// compared by nil-ness too, since nil means the default argv.
func sameEngine(a, b Model) bool {
	return a.Name == b.Name && a.Reference == b.Reference && a.EngineImage == b.EngineImage &&
		slices.Equal(a.EngineArgs, b.EngineArgs) &&
		(a.Command == nil) == (b.Command == nil) && slices.Equal(a.Command, b.Command) &&
		a.ContainerPort == b.ContainerPort && a.MemoryMB == b.MemoryMB && a.VGPUs == b.VGPUs
}

const modelSelect = `SELECT id, name, reference, engine_image, engine_args, command, container_port,
	memory_mb, vgpus, auto, labels, created_at, updated_at FROM models `

type scanner interface {
	Scan(dest ...any) error
}

func getModel(ctx context.Context, q store.Querier, where, arg string) (Model, error) {
	mod, err := scanModel(q.QueryRowContext(ctx, modelSelect+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, httpx.NotFound("model %q not found", arg)
	}
	return mod, err
}

func scanModel(s scanner) (Model, error) {
	var (
		mod          Model
		args, labels string
		command      sql.NullString
		auto         int
	)
	err := s.Scan(&mod.ID, &mod.Name, &mod.Reference, &mod.EngineImage, &args, &command,
		&mod.ContainerPort, &mod.MemoryMB, &mod.VGPUs, &auto, &labels, &mod.CreatedAt, &mod.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Model{}, err
		}
		return Model{}, fmt.Errorf("deploy: read model: %w", err)
	}
	mod.Auto = auto != 0
	if err := json.Unmarshal([]byte(args), &mod.EngineArgs); err != nil {
		return Model{}, fmt.Errorf("deploy: model %s engine_args: %w", mod.ID, err)
	}
	if command.Valid {
		if err := json.Unmarshal([]byte(command.String), &mod.Command); err != nil {
			return Model{}, fmt.Errorf("deploy: model %s command: %w", mod.ID, err)
		}
	}
	if err := json.Unmarshal([]byte(labels), &mod.Labels); err != nil {
		return Model{}, fmt.Errorf("deploy: model %s labels: %w", mod.ID, err)
	}
	if mod.EngineArgs == nil {
		mod.EngineArgs = []string{}
	}
	if mod.Labels == nil {
		mod.Labels = map[string]string{}
	}
	return mod, nil
}

// modelColumns returns the column values in table order, id first.
func modelColumns(mod Model) ([]any, error) {
	args, err := json.Marshal(mod.EngineArgs)
	if err != nil {
		return nil, fmt.Errorf("deploy: encode engine_args: %w", err)
	}
	labels, err := json.Marshal(mod.Labels)
	if err != nil {
		return nil, fmt.Errorf("deploy: encode labels: %w", err)
	}
	var command sql.NullString
	if mod.Command != nil {
		b, err := json.Marshal(mod.Command)
		if err != nil {
			return nil, fmt.Errorf("deploy: encode command: %w", err)
		}
		command = sql.NullString{String: string(b), Valid: true}
	}
	auto := 0
	if mod.Auto {
		auto = 1
	}
	return []any{mod.ID, mod.Name, mod.Reference, mod.EngineImage, string(args), command,
		mod.ContainerPort, mod.MemoryMB, mod.VGPUs, auto, string(labels), mod.CreatedAt, mod.UpdatedAt}, nil
}

func countDeployments(ctx context.Context, q store.Querier, modelID string) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployments WHERE model_id = ?`, modelID).Scan(&n); err != nil {
		return 0, fmt.Errorf("deploy: count deployments: %w", err)
	}
	return n, nil
}

// isUniqueViolation matches SQLite's UNIQUE constraint error by message:
// the driver's extended code would tie this package to modernc internals
// for no gain.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
