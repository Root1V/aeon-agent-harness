package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ToolInvocations is INT-013's bitácora: one append-only row per tool invocation, written whether
// or not the call belongs to a run. See migrations/0004_tool_invocations.sql for why this is not
// `tool_executions` (that one is the idempotency claim, keyed by the caller's key) and why it has
// no cost column yet.
type ToolInvocations struct {
	pool   *pgxpool.Pool
	tenant string
}

// ToolInvocationsFor returns the log handle SCOPED TO ONE TENANT, the same shape as
// ToolExecutionsFor and for the same reason: a handle that cannot exist without a tenant makes
// "did this query remember?" something the compiler answers once (VRT-AEON-005 T-3).
func (s *Store) ToolInvocationsFor(tenant string) *ToolInvocations {
	return &ToolInvocations{pool: s.pool, tenant: tenant}
}

// Outcome values: the four ways a tool call is resolved. `ok`/`error` are written from inside
// Execute (INT-013); `denied`/`replayed` are written by the doors (INT-014), because neither fact
// passes through Execute and the one chokepoint that would cover them was rejected on purpose —
// see migrations/0005 for why an audit gap was preferred to an enforcement branch in the executor.
const (
	ToolInvocationOK    = "ok"
	ToolInvocationError = "error"
	// INT-014: the two resolutions that never reach Execute. A denial is decided strictly before it
	// (ADR-0001), and a replay is served from the dedupe cache without executing.
	ToolInvocationDenied   = "denied"
	ToolInvocationReplayed = "replayed"
)

// ToolInvocation is one row. Attribution fields are empty rather than absent when the call has no
// run: an external MCP caller is not a run, and the empty string says exactly that.
type ToolInvocation struct {
	ToolName         string
	Door             string
	Outcome          string
	ErrorMessage     string // empty unless Outcome is ToolInvocationError
	RunID            string
	StepID           string
	AgentManifestRef string
	// DurationMS is nil when nothing executed — a denial or a replay. Writing 0 would say "it ran
	// and took no time", which is the same lie OBS-008 removed from cost_usd.
	DurationMS *int64
	// Disposition is INT-010's vocabulary and is REQUIRED on a denial (the database enforces it):
	// "refused" and "refused, but a person could approve this" are different facts, and an audit
	// that conflates them cannot say why a run stopped.
	Disposition string
	PolicyID    string
}

// Record appends one invocation.
//
// It takes its own context rather than the caller's on purpose at the call site in toolexec — see
// the comment there. Here the only rule is that a row that would lie is refused by the CHECK
// constraints rather than silently stored, so a bug in a caller surfaces as a failed write.
func (l *ToolInvocations) Record(ctx context.Context, inv ToolInvocation) error {
	var errMessage *string
	if inv.Outcome == ToolInvocationError {
		msg := inv.ErrorMessage
		if msg == "" {
			// The CHECK would reject NULL here, and an empty string would record "failed without a
			// message". Neither is better than naming the gap, so the row stays truthful.
			msg = "(the executor returned an error with an empty message)"
		}
		errMessage = &msg
	}

	_, err := l.pool.Exec(ctx,
		`INSERT INTO tool_invocations
		   (tenant_id, tool_name, door, outcome, error_message, run_id, step_id, agent_manifest_ref,
		    duration_ms, disposition, policy_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		l.tenant, inv.ToolName, inv.Door, inv.Outcome, errMessage,
		inv.RunID, inv.StepID, inv.AgentManifestRef, inv.DurationMS, inv.Disposition, inv.PolicyID,
	)
	if err != nil {
		return fmt.Errorf("store: record tool invocation: %w", err)
	}
	return nil
}

// ForRun returns this tenant's invocations for one run, newest first. The tenant comes from the
// handle, so a caller cannot read another tenant's bitácora by passing a run id it guessed.
func (l *ToolInvocations) ForRun(ctx context.Context, runID string, limit int) ([]ToolInvocation, error) {
	return l.query(ctx,
		`SELECT tool_name, door, outcome, COALESCE(error_message, ''), run_id, step_id,
		        agent_manifest_ref, duration_ms, disposition, policy_id
		   FROM tool_invocations
		  WHERE tenant_id = $1 AND run_id = $2
		  ORDER BY occurred_at DESC, id DESC
		  LIMIT $3`,
		l.tenant, runID, limit)
}

// ForTool returns this tenant's invocations of one tool, newest first — the other question an
// audit actually asks.
func (l *ToolInvocations) ForTool(ctx context.Context, toolName string, limit int) ([]ToolInvocation, error) {
	return l.query(ctx,
		`SELECT tool_name, door, outcome, COALESCE(error_message, ''), run_id, step_id,
		        agent_manifest_ref, duration_ms, disposition, policy_id
		   FROM tool_invocations
		  WHERE tenant_id = $1 AND tool_name = $2
		  ORDER BY occurred_at DESC, id DESC
		  LIMIT $3`,
		l.tenant, toolName, limit)
}

func (l *ToolInvocations) query(ctx context.Context, sql string, args ...any) ([]ToolInvocation, error) {
	rows, err := l.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read tool invocations: %w", err)
	}
	defer rows.Close()

	var out []ToolInvocation
	for rows.Next() {
		var inv ToolInvocation
		if err := rows.Scan(&inv.ToolName, &inv.Door, &inv.Outcome, &inv.ErrorMessage,
			&inv.RunID, &inv.StepID, &inv.AgentManifestRef, &inv.DurationMS,
			&inv.Disposition, &inv.PolicyID); err != nil {
			return nil, fmt.Errorf("store: scan tool invocation: %w", err)
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read tool invocations: %w", err)
	}
	return out, nil
}
