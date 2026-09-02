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

// ErrReleaseGateBlocked is returned when a Candidate -> Released transition is requested with a
// ReleaseGateDecision that didn't allow it (EVAL-003).
var ErrReleaseGateBlocked = errors.New("store: release gate blocked promotion to Released")

// ErrNotReleased is returned by Quarantine when the target agent version isn't (or is no longer)
// Released — quarantine is a control on live, in-production traffic; a Draft/Candidate/Retired
// version was never (or is no longer) reachable to begin with (A5).
var ErrNotReleased = errors.New("store: agent version is not Released")

// ReleaseGateDecision is EVAL-003's typed verdict on whether a Candidate may be promoted to
// Released — computed elsewhere (aeon_evalops.release_gate.evaluate_release_gate, comparing the
// candidate's eval results against the currently Released version's own baseline) and passed in
// here. This store has no way to run an eval suite itself and shouldn't grow one; it only applies
// the decision, the same separation of concerns as a Decision applied by a deterministic workflow
// (docs/adr/0001). Ignored for every transition except Candidate -> Released.
type ReleaseGateDecision struct {
	Allowed bool
	Reason  string // surfaced in the error when Allowed is false
}

// AgentRecord is a stored AgentManifest plus registry metadata (FND-001).
type AgentRecord struct {
	Name             string         `json:"name"`
	Version          string         `json:"version"`
	Owner            string         `json:"owner"`
	Lifecycle        string         `json:"lifecycle"`
	Manifest         map[string]any `json:"manifest"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	Quarantined      bool           `json:"quarantined"`
	QuarantineReason string         `json:"quarantine_reason,omitempty"`
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
		`SELECT name, version, owner, lifecycle, manifest, created_at, updated_at, quarantined, quarantine_reason
		 FROM agents WHERE name = $1 AND version = $2`,
		name, version,
	)
	return scanAgentRow(row)
}

// List returns every registered version of every agent, newest first.
func (r *AgentRegistry) List(ctx context.Context) ([]*AgentRecord, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT name, version, owner, lifecycle, manifest, created_at, updated_at, quarantined, quarantine_reason
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
// backward, or requesting the current state — returns ErrInvalidTransition. The Candidate ->
// Released step additionally requires gate.Allowed (EVAL-003's Release Gate) — every other
// transition ignores gate entirely, so callers not promoting to Released may pass the zero value.
func (r *AgentRegistry) TransitionLifecycle(ctx context.Context, name, version, target string, gate ReleaseGateDecision) (*AgentRecord, error) {
	current, err := r.Get(ctx, name, version)
	if err != nil {
		return nil, err
	}
	allowed, ok := validTransitions[current.Lifecycle]
	if !ok || allowed != target {
		return nil, fmt.Errorf("%w: %s@%s is %s, cannot move to %s (only %s is allowed)",
			ErrInvalidTransition, name, version, current.Lifecycle, target, allowed)
	}

	if current.Lifecycle == LifecycleCandidate && target == LifecycleReleased && !gate.Allowed {
		reason := gate.Reason
		if reason == "" {
			reason = "no passing release gate decision was provided"
		}
		return nil, fmt.Errorf("%w: %s@%s: %s", ErrReleaseGateBlocked, name, version, reason)
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

// Quarantine immediately marks a Released agent version quarantined, with reason recorded for
// operators — the automatic circuit-breaker trip and the manual "kill switch" are the same
// operation here, just different callers (see go/internal/circuitbreaker). Deliberately NOT part of
// validTransitions/TransitionLifecycle: quarantine is an orthogonal flag on a Released version, not
// a lifecycle move — the agent stays Released throughout, satisfying "forward-only" untouched.
// Idempotent: quarantining an already-quarantined version just updates the reason.
func (r *AgentRegistry) Quarantine(ctx context.Context, name, version, reason string) (*AgentRecord, error) {
	current, err := r.Get(ctx, name, version)
	if err != nil {
		return nil, err
	}
	if current.Lifecycle != LifecycleReleased {
		return nil, fmt.Errorf("%w: %s@%s is %s", ErrNotReleased, name, version, current.Lifecycle)
	}
	_, err = r.pool.Exec(ctx,
		`UPDATE agents SET quarantined = TRUE, quarantine_reason = $1, updated_at = now() WHERE name = $2 AND version = $3`,
		reason, name, version,
	)
	if err != nil {
		return nil, fmt.Errorf("store: quarantine: %w", err)
	}
	return r.Get(ctx, name, version)
}

// Unquarantine clears a version's quarantine — the manual reset after remediation. Safe to call on
// a version that isn't currently quarantined (a no-op update).
func (r *AgentRegistry) Unquarantine(ctx context.Context, name, version string) (*AgentRecord, error) {
	if _, err := r.Get(ctx, name, version); err != nil {
		return nil, err
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE agents SET quarantined = FALSE, quarantine_reason = NULL, updated_at = now() WHERE name = $1 AND version = $2`,
		name, version,
	)
	if err != nil {
		return nil, fmt.Errorf("store: unquarantine: %w", err)
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
	var quarantineReason *string
	err := row.Scan(&rec.Name, &rec.Version, &rec.Owner, &rec.Lifecycle, &manifestJSON, &rec.CreatedAt, &rec.UpdatedAt, &rec.Quarantined, &quarantineReason)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: scan agent: %w", err)
	}
	if quarantineReason != nil {
		rec.QuarantineReason = *quarantineReason
	}
	if err := json.Unmarshal(manifestJSON, &rec.Manifest); err != nil {
		return nil, fmt.Errorf("store: unmarshal manifest: %w", err)
	}
	return &rec, nil
}
