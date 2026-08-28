package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Lifecycle states for an AgentManifest (FND-001). Forward-only: a manifest moves through these
// in order and can never skip a state or move backward. This mirrors proto/manifests/
// agent_manifest.schema.json's metadata.lifecycle enum.
const (
	LifecycleDraft     = "Draft"
	LifecycleCandidate = "Candidate"
	LifecycleReleased  = "Released"
	LifecycleRetired   = "Retired"
)

// validTransitions maps a current lifecycle state to the single state it may advance to.
// Anything not listed here (including staying put, skipping a state, or moving backward) is
// rejected by TransitionLifecycle.
var validTransitions = map[string]string{
	LifecycleDraft:     LifecycleCandidate,
	LifecycleCandidate: LifecycleReleased,
	LifecycleReleased:  LifecycleRetired,
}

// ErrNotFound is returned when a lookup finds no matching row.
var ErrNotFound = errors.New("store: not found")

// ErrInvalidTransition is returned when a requested lifecycle transition is not the single
// allowed next step for the record's current state.
var ErrInvalidTransition = errors.New("store: invalid lifecycle transition")

// ErrAlreadyExists is returned by Create when (name, version) is already registered — versions
// are immutable once created; a new version is how you change a Released agent.
var ErrAlreadyExists = errors.New("store: already exists")

// AgentRecord is a stored AgentManifest plus registry metadata (FND-001).
type AgentRecord struct {
	Name      string         `json:"name"`
	Version   string         `json:"version"`
	Owner     string         `json:"owner"`
	Lifecycle string         `json:"lifecycle"`
	Manifest  map[string]any `json:"manifest"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// AgentRegistry is the Postgres-backed CRUD + lifecycle store for AgentManifests.
type AgentRegistry struct {
	pool *pgxpool.Pool
}

// Create registers a new AgentManifest version. Fails with ErrAlreadyExists if (name, version)
// is already registered — per FND-003, a manifest is immutable once created; publish a new
// version instead of mutating one in place.
func (r *AgentRegistry) Create(ctx context.Context, manifest map[string]any, owner string) (*AgentRecord, error) {
	name, version, err := manifestNameVersion(manifest)
	if err != nil {
		return nil, err
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("store: marshal manifest: %w", err)
	}

	_, err = r.pool.Exec(ctx,
		`INSERT INTO agents (name, version, owner, lifecycle, manifest) VALUES ($1, $2, $3, $4, $5)`,
		name, version, owner, LifecycleDraft, manifestJSON,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: agent %s@%s", ErrAlreadyExists, name, version)
		}
		return nil, fmt.Errorf("store: create agent: %w", err)
	}
	return r.Get(ctx, name, version)
}

// Get fetches a single agent version.
func (r *AgentRegistry) Get(ctx context.Context, name, version string) (*AgentRecord, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT name, version, owner, lifecycle, manifest, created_at, updated_at
		 FROM agents WHERE name = $1 AND version = $2`,
		name, version,
	)
	return scanAgentRow(row)
}

// List returns every registered version of every agent, newest first.
func (r *AgentRegistry) List(ctx context.Context) ([]*AgentRecord, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT name, version, owner, lifecycle, manifest, created_at, updated_at
		 FROM agents ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list agents: %w", err)
	}
	defer rows.Close()

	var out []*AgentRecord
	for rows.Next() {
		rec, err := scanAgentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// TransitionLifecycle moves an agent version forward exactly one lifecycle step (Draft ->
// Candidate -> Released -> Retired). Any other requested target — skipping a state, moving
// backward, or requesting the current state — returns ErrInvalidTransition. This is what
// eventually backs Release Gates (EVAL-003): a controlled, auditable, one-step-at-a-time path
// to Released.
func (r *AgentRegistry) TransitionLifecycle(ctx context.Context, name, version, target string) (*AgentRecord, error) {
	current, err := r.Get(ctx, name, version)
	if err != nil {
		return nil, err
	}
	allowed, ok := validTransitions[current.Lifecycle]
	if !ok || allowed != target {
		return nil, fmt.Errorf("%w: %s@%s is %s, cannot move to %s (only %s is allowed)",
			ErrInvalidTransition, name, version, current.Lifecycle, target, allowed)
	}

	_, err = r.pool.Exec(ctx,
		`UPDATE agents SET lifecycle = $1, updated_at = now() WHERE name = $2 AND version = $3`,
		target, name, version,
	)
	if err != nil {
		return nil, fmt.Errorf("store: transition lifecycle: %w", err)
	}
	return r.Get(ctx, name, version)
}

func manifestNameVersion(manifest map[string]any) (name, version string, err error) {
	metadata, ok := manifest["metadata"].(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("store: manifest missing metadata object")
	}
	name, _ = metadata["name"].(string)
	version, _ = metadata["version"].(string)
	if name == "" || version == "" {
		return "", "", fmt.Errorf("store: manifest metadata.name and metadata.version are required")
	}
	return name, version, nil
}

// rowScanner abstracts pgx.Row vs pgx.Rows so scanAgentRow works for both Get (single row) and
// List (iterating rows) without duplicating the Scan call and JSON unmarshal.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAgentRow(row rowScanner) (*AgentRecord, error) {
	var rec AgentRecord
	var manifestJSON []byte
	err := row.Scan(&rec.Name, &rec.Version, &rec.Owner, &rec.Lifecycle, &manifestJSON, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: scan agent: %w", err)
	}
	if err := json.Unmarshal(manifestJSON, &rec.Manifest); err != nil {
		return nil, fmt.Errorf("store: unmarshal manifest: %w", err)
	}
	return &rec, nil
}
