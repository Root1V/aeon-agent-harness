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

// Side-effect classes for a ToolDescriptor (TOOL-001), mirroring
// proto/schemas/tool_descriptor.schema.json.
const (
	SideEffectNone              = "NONE"
	SideEffectReadOnly          = "READ_ONLY"
	SideEffectWriteReversible   = "WRITE_REVERSIBLE"
	SideEffectWriteIrreversible = "WRITE_IRREVERSIBLE"
)

// Risk levels for a ToolDescriptor.
const (
	RiskLow      = "LOW"
	RiskMedium   = "MEDIUM"
	RiskHigh     = "HIGH"
	RiskCritical = "CRITICAL"
)

// ToolRecord is a stored ToolDescriptor plus registry metadata (TOOL-001).
type ToolRecord struct {
	ToolID     string         `json:"tool_id"`
	Version    string         `json:"version"`
	Name       string         `json:"name"`
	SideEffect string         `json:"side_effect"`
	Risk       string         `json:"risk"`
	Descriptor map[string]any `json:"descriptor"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// ToolRegistry is the Postgres-backed CRUD store for ToolDescriptors.
type ToolRegistry struct {
	pool *pgxpool.Pool
}

// Create registers a new tool version. Enforces the roadmap A-adjacent rule that any tool with a
// side effect beyond READ_ONLY must declare idempotency_key_fields (see docs/adr/0001 —
// idempotency is what makes crash-resume safe; a tool that can't be deduplicated has no business
// being WRITE_REVERSIBLE/WRITE_IRREVERSIBLE without an explicit key).
func (r *ToolRegistry) Create(ctx context.Context, descriptor map[string]any) (*ToolRecord, error) {
	toolID, version, name, sideEffect, risk, err := validateToolDescriptor(descriptor)
	if err != nil {
		return nil, err
	}

	descriptorJSON, err := json.Marshal(descriptor)
	if err != nil {
		return nil, fmt.Errorf("store: marshal tool descriptor: %w", err)
	}

	_, err = r.pool.Exec(ctx,
		`INSERT INTO tools (tool_id, version, name, side_effect, risk, descriptor)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		toolID, version, name, sideEffect, risk, descriptorJSON,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: tool %s@%s", ErrAlreadyExists, toolID, version)
		}
		return nil, fmt.Errorf("store: create tool: %w", err)
	}
	return r.Get(ctx, toolID, version)
}

// Get fetches a single tool version.
func (r *ToolRegistry) Get(ctx context.Context, toolID, version string) (*ToolRecord, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT tool_id, version, name, side_effect, risk, descriptor, created_at, updated_at
		 FROM tools WHERE tool_id = $1 AND version = $2`,
		toolID, version,
	)
	return scanToolRow(row)
}

// List returns every registered tool version, newest first.
func (r *ToolRegistry) List(ctx context.Context) ([]*ToolRecord, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT tool_id, version, name, side_effect, risk, descriptor, created_at, updated_at
		 FROM tools ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list tools: %w", err)
	}
	defer rows.Close()

	var out []*ToolRecord
	for rows.Next() {
		rec, err := scanToolRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func validateToolDescriptor(d map[string]any) (toolID, version, name, sideEffect, risk string, err error) {
	toolID, _ = d["tool_id"].(string)
	version, _ = d["version"].(string)
	name, _ = d["name"].(string)
	sideEffect, _ = d["side_effect"].(string)
	risk, _ = d["risk"].(string)

	if toolID == "" || version == "" || name == "" {
		return "", "", "", "", "", fmt.Errorf("store: tool_id, version and name are required")
	}
	if !oneOf(sideEffect, SideEffectNone, SideEffectReadOnly, SideEffectWriteReversible, SideEffectWriteIrreversible) {
		return "", "", "", "", "", fmt.Errorf("store: invalid side_effect %q", sideEffect)
	}
	if !oneOf(risk, RiskLow, RiskMedium, RiskHigh, RiskCritical) {
		return "", "", "", "", "", fmt.Errorf("store: invalid risk %q", risk)
	}
	if sideEffect != SideEffectNone && sideEffect != SideEffectReadOnly {
		fields, _ := d["idempotency_key_fields"].([]any)
		if len(fields) == 0 {
			return "", "", "", "", "", fmt.Errorf(
				"store: tool %q has side_effect=%s but no idempotency_key_fields — a tool with "+
					"side effects must declare how it dedupes retries (see docs/adr/0001)", name, sideEffect)
		}
	}
	return toolID, version, name, sideEffect, risk, nil
}

func oneOf(v string, options ...string) bool {
	for _, o := range options {
		if v == o {
			return true
		}
	}
	return false
}

func scanToolRow(row rowScanner) (*ToolRecord, error) {
	var rec ToolRecord
	var descriptorJSON []byte
	err := row.Scan(&rec.ToolID, &rec.Version, &rec.Name, &rec.SideEffect, &rec.Risk, &descriptorJSON, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: scan tool: %w", err)
	}
	if err := json.Unmarshal(descriptorJSON, &rec.Descriptor); err != nil {
		return nil, fmt.Errorf("store: unmarshal descriptor: %w", err)
	}
	return &rec, nil
}
