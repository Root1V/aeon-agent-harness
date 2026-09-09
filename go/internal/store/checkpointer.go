package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aeon-ai/aeon/go/internal/checkpoint"
)

// Checkpointer is the Postgres-backed implementation of INT-009's durability seam
// (checkpoint.Checkpointer). See go/internal/checkpoint for the contract and schema.sql for the DDL.
type Checkpointer struct {
	pool *pgxpool.Pool
}

var _ checkpoint.Checkpointer = (*Checkpointer)(nil)

// Append journals one entry, idempotently by (run_id, step_id, phase).
//
// The whole call runs in one transaction holding a per-run advisory lock. That lock is not about
// the duplicate check — the primary key already makes double-writes impossible — it is about `seq`:
// two concurrent appends for the same run (routine with parallel graph nodes) would otherwise both
// read the same MAX(seq) and one would lose to the (run_id, seq) unique constraint. Serialising per
// run turns a retry loop into a wait. Different runs never contend, except for the occasional
// hashtext collision, which costs a little contention and nothing else.
func (c *Checkpointer) Append(ctx context.Context, entry checkpoint.Entry) (checkpoint.AppendResult, error) {
	if err := entry.Validate(); err != nil {
		return checkpoint.AppendResult{}, err
	}

	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return checkpoint.AppendResult{}, fmt.Errorf("store: begin checkpoint append: %w", err)
	}
	defer tx.Rollback(context.Background())

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, entry.RunID); err != nil {
		return checkpoint.AppendResult{}, fmt.Errorf("store: locking run %s for append: %w", entry.RunID, err)
	}

	payload := payloadArg(entry.Payload)

	// Comparison is `IS NOT DISTINCT FROM` on jsonb, so it is semantic, not byte-wise: a retry that
	// re-serialises the same object with its keys in a different order is correctly *not* a
	// divergence. That matters because the caller on the other side of this seam is Python, where
	// dict ordering across attempts is not something we get to assume.
	var existingSeq int64
	var same bool
	err = tx.QueryRow(ctx,
		`SELECT seq, payload IS NOT DISTINCT FROM $4::jsonb
		   FROM run_checkpoints WHERE run_id = $1 AND step_id = $2 AND phase = $3`,
		entry.RunID, entry.StepID, string(entry.Phase), payload,
	).Scan(&existingSeq, &same)
	switch {
	case err == nil:
		if err := tx.Commit(ctx); err != nil {
			return checkpoint.AppendResult{}, fmt.Errorf("store: commit duplicate checkpoint append: %w", err)
		}
		return checkpoint.AppendResult{Seq: existingSeq, Duplicate: true, PayloadDiverged: !same}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return checkpoint.AppendResult{}, fmt.Errorf("store: reading existing checkpoint: %w", err)
	}

	var seq int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO run_checkpoints (run_id, step_id, phase, seq, payload)
		 SELECT $1, $2, $3, COALESCE(MAX(seq), -1) + 1, $4::jsonb FROM run_checkpoints WHERE run_id = $1
		 RETURNING seq`,
		entry.RunID, entry.StepID, string(entry.Phase), payload,
	).Scan(&seq); err != nil {
		return checkpoint.AppendResult{}, fmt.Errorf("store: appending checkpoint: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return checkpoint.AppendResult{}, fmt.Errorf("store: commit checkpoint append: %w", err)
	}
	return checkpoint.AppendResult{Seq: seq}, nil
}

// Load reads the run's whole journal and reconstructs the state.
func (c *Checkpointer) Load(ctx context.Context, runID string) (*checkpoint.RunState, error) {
	if runID == "" {
		return nil, fmt.Errorf("store: run_id is required to load checkpoints")
	}
	rows, err := c.pool.Query(ctx,
		`SELECT step_id, phase, seq, payload, recorded_at FROM run_checkpoints WHERE run_id = $1 ORDER BY seq`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: loading checkpoints for run %s: %w", runID, err)
	}
	defer rows.Close()

	var records []checkpoint.Record
	for rows.Next() {
		var rec checkpoint.Record
		var phase string
		var payload []byte
		if err := rows.Scan(&rec.StepID, &phase, &rec.Seq, &payload, &rec.RecordedAt); err != nil {
			return nil, fmt.Errorf("store: scanning checkpoint: %w", err)
		}
		rec.RunID = runID
		rec.Phase = checkpoint.Phase(phase)
		if payload != nil {
			rec.Payload = json.RawMessage(payload)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading checkpoints for run %s: %w", runID, err)
	}
	return checkpoint.NewRunState(runID, records), nil
}

// payloadArg maps an absent payload to SQL NULL rather than to the JSON literal `null`. An
// attempted-phase entry usually has no result yet, and "no payload recorded" is a different fact
// from "the recorded result was null".
func payloadArg(p json.RawMessage) any {
	if len(p) == 0 {
		return nil
	}
	return string(p)
}
