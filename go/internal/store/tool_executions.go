package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ToolExecutions is TOOL-005: the execution dedupe table the Tool Gateway consults before running
// anything with effects. See schema.sql for the DDL and why the state has three values.
type ToolExecutions struct {
	pool *pgxpool.Pool
}

// ErrExecutionInFlight means another caller holds this idempotency key and has not finished. It is
// deliberately NOT "go ahead": the whole point of the table is that two workers retrying the same
// Activity cannot both execute. Mirrors what the Prometheus platform does with its own idempotency
// keys — a typed, retryable rejection rather than a wait, because holding the connection open makes
// the caller's timeout the thing that decides.
var ErrExecutionInFlight = errors.New("store: an execution with this idempotency key is already in flight")

// ClaimResult says what the caller should do next. Exactly one of the three is true.
type ClaimResult struct {
	// Completed carries the recorded result of an execution that already happened. The caller must
	// NOT execute: returning this is the deduplication.
	Completed json.RawMessage
	// Claimed is true when this caller now owns the key and must execute.
	Claimed bool
	// ArgsDiverged reports that the key was already recorded against DIFFERENT arguments. The key is
	// supposed to be derived from the arguments, so this means two different calls collided on one
	// key — a derivation bug somewhere, and far more dangerous than a duplicate: it would return one
	// call's result to another. Same reasoning as INT-009's payload_diverged, with a sharper edge.
	ArgsDiverged bool
	// FailedAttempts is how many previous attempts ended in error. Non-zero means the effect may
	// have landed before one of them failed.
	FailedAttempts int
}

// ToolExecutions returns the TOOL-005 dedupe handle.
func (s *Store) ToolExecutions() *ToolExecutions {
	return &ToolExecutions{pool: s.pool}
}

// Claim atomically takes ownership of an idempotency key, or reports why the caller cannot have it.
//
// The claim is written BEFORE execution on purpose. Recording afterwards would leave a window where
// a crash loses the record and the retry runs the effect a second time — which is the exact failure
// this table exists to prevent, and the reason a "write the result when done" design looks correct
// and is not.
func (e *ToolExecutions) Claim(ctx context.Context, key, toolName, agentRef string, args map[string]any) (ClaimResult, error) {
	if key == "" {
		return ClaimResult{}, fmt.Errorf("store: idempotency key is required")
	}
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("store: encoding tool args: %w", err)
	}

	var claimed bool
	err = e.pool.QueryRow(ctx,
		`INSERT INTO tool_executions (idempotency_key, tool_name, agent_manifest_ref, args)
		 VALUES ($1, $2, $3, $4::jsonb)
		 ON CONFLICT (idempotency_key) DO NOTHING
		 RETURNING true`,
		key, toolName, agentRef, string(encodedArgs),
	).Scan(&claimed)
	switch {
	case err == nil:
		return ClaimResult{Claimed: true}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return ClaimResult{}, fmt.Errorf("store: claiming idempotency key: %w", err)
	}

	// The key existed. Which of the three states is it in?
	var result []byte
	var state string
	var sameArgs bool
	var failedAttempts int
	if err := e.pool.QueryRow(ctx,
		`SELECT result, state, args IS NOT DISTINCT FROM $2::jsonb, failed_attempts
		   FROM tool_executions WHERE idempotency_key = $1`,
		key, string(encodedArgs),
	).Scan(&result, &state, &sameArgs, &failedAttempts); err != nil {
		return ClaimResult{}, fmt.Errorf("store: reading existing execution: %w", err)
	}

	out := ClaimResult{ArgsDiverged: !sameArgs, FailedAttempts: failedAttempts}
	switch state {
	case "completed":
		out.Completed = result
		return out, nil
	case "released":
		// A previous attempt failed and gave the key back. Re-claiming is conditional on the state
		// still being "released", so two retries racing here cannot both win.
		var reclaimed bool
		err := e.pool.QueryRow(ctx,
			`UPDATE tool_executions SET state = 'in_flight', claimed_at = now()
			   WHERE idempotency_key = $1 AND state = 'released'
			 RETURNING true`,
			key,
		).Scan(&reclaimed)
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrExecutionInFlight
		}
		if err != nil {
			return ClaimResult{}, fmt.Errorf("store: re-claiming released key: %w", err)
		}
		out.Claimed = true
		return out, nil
	default:
		return out, ErrExecutionInFlight
	}
}

// Complete records the result, which is what turns a claim into a deduplicated answer.
func (e *ToolExecutions) Complete(ctx context.Context, key string, result map[string]any) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("store: encoding tool result: %w", err)
	}
	if _, err := e.pool.Exec(ctx,
		`UPDATE tool_executions SET result = $2::jsonb, state = 'completed', completed_at = now()
		   WHERE idempotency_key = $1`,
		key, string(encoded),
	); err != nil {
		return fmt.Errorf("store: completing execution: %w", err)
	}
	return nil
}

// Release gives the key back after a failed execution, counting the failure.
//
// Releasing is a judgement call and worth stating: a sticky failure would make one transient error
// permanent for that step forever, which is worse in the common case. The uncommon case — an effect
// that landed before the error — is why failed_attempts is kept rather than thrown away: the next
// claim can see that someone was here before instead of believing it is the first, and a caller
// whose tool is not safe to repeat has the one fact it needs to refuse.
func (e *ToolExecutions) Release(ctx context.Context, key string) error {
	if _, err := e.pool.Exec(ctx,
		`UPDATE tool_executions
		    SET state = 'released', failed_attempts = failed_attempts + 1, result = NULL, completed_at = NULL
		  WHERE idempotency_key = $1`,
		key,
	); err != nil {
		return fmt.Errorf("store: releasing execution claim: %w", err)
	}
	return nil
}
