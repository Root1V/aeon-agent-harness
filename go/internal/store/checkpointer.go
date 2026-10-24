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
// (checkpoint.Checkpointer). See go/internal/checkpoint for the contract and migrations/ for the DDL.
type Checkpointer struct {
	pool   *pgxpool.Pool
	tenant string
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
		   FROM run_checkpoints
		  WHERE tenant_id = $5 AND run_id = $1 AND sub_run_id = $6 AND step_id = $2 AND phase = $3`,
		entry.RunID, entry.StepID, string(entry.Phase), payload, c.tenant, entry.SubRunID,
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
		// MAX(seq) is over the whole RUN and not over the sub-run, deliberately: NextSeq answers
		// "where does this run's journal continue", and one sequence per run keeps a delegation's
		// entries ordered against its parent's. Per-sub-run numbering would produce two entries
		// sharing a seq, which is the one thing the ordering relies on not happening.
		`INSERT INTO run_checkpoints (tenant_id, run_id, sub_run_id, step_id, phase, seq, payload)
		 SELECT $5, $1, $6, $2, $3, COALESCE(MAX(seq), -1) + 1, $4::jsonb
		   FROM run_checkpoints WHERE tenant_id = $5 AND run_id = $1
		 RETURNING seq`,
		entry.RunID, entry.StepID, string(entry.Phase), payload, c.tenant, entry.SubRunID,
	).Scan(&seq); err != nil {
		return checkpoint.AppendResult{}, fmt.Errorf("store: appending checkpoint: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return checkpoint.AppendResult{}, fmt.Errorf("store: commit checkpoint append: %w", err)
	}
	return checkpoint.AppendResult{Seq: seq}, nil
}

// Load reads the run's whole journal and reconstructs the state.
// Load returns ONE sub-run's journal, with the run-wide next sequence (INT-009c).
//
// SUB-RUN IS A REQUIRED ARGUMENT AND NOT AN OPTION, which is the point of the signature. Veritium
// found that this read ignored it entirely: the write has stored `sub_run_id` and keyed on it since
// INT-009b, the contract schema declares it, and the read returned EVERY sub-run of the parent — so
// a sub-run resuming would adopt its siblings' steps as its own. Making it a parameter means the one
// caller had to decide, rather than inheriting a default that was wrong for anyone with sub-runs.
//
// EMPTY MEANS THE ROOT RUN, with no special case anywhere: the write stores `”` for a root step, so
// filtering is plain equality and "the parameter was omitted" lands on the root rows by itself.
// KnownRun reports whether this tenant's journal holds ANY entry for the run, in any sub-run.
//
// FIN-002, and it exists to keep two facts apart that a cost query cannot tell apart on its own: a run
// that spent nothing and a run id that does not exist. The ledger has no row for either, and there is
// no `runs` table — a run's existence lives in Temporal, not here. The journal is the next best
// witness and it is in the same database: a run that did anything durable left an entry.
//
// Asking Temporal instead would be more accurate and worse: a cost read would then fail whenever
// Temporal is unreachable, which is the moment someone most wants to know what a run already spent.
//
// A run refused before its first model call is the case that makes this worth a query. Reporting it
// as 404 would make a governed-and-refused run indistinguishable from a typo, and the refusal is
// precisely the thing an operator is looking for.
func (c *Checkpointer) KnownRun(ctx context.Context, runID string) (bool, error) {
	if runID == "" {
		return false, fmt.Errorf("store: run_id is required")
	}
	var known bool
	if err := c.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM run_checkpoints WHERE tenant_id = $2 AND run_id = $1)`,
		runID, c.tenant,
	).Scan(&known); err != nil {
		return false, fmt.Errorf("store: checking whether run %s is known: %w", runID, err)
	}
	return known, nil
}

func (c *Checkpointer) Load(ctx context.Context, runID, subRunID string) (*checkpoint.RunState, error) {
	if runID == "" {
		return nil, fmt.Errorf("store: run_id is required to load checkpoints")
	}
	rows, err := c.pool.Query(ctx,
		`SELECT sub_run_id, step_id, phase, seq, payload, recorded_at
		   FROM run_checkpoints WHERE tenant_id = $2 AND run_id = $1 ORDER BY seq`,
		runID, c.tenant,
	)
	if err != nil {
		return nil, fmt.Errorf("store: loading checkpoints for run %s: %w", runID, err)
	}
	defer rows.Close()

	// EVERY ROW IS READ AND ONLY THE MATCHING ONES ARE KEPT, on purpose rather than filtering in SQL.
	// `next_seq` has to stay RUN-WIDE (INT-009b): it orders the journal across sub-runs, so deriving
	// it from one sub-run's rows would give a resuming sub-run numbers its siblings already used. One
	// round trip answers both questions; a `WHERE sub_run_id = $3` would answer the wrong one
	// correctly.
	var records []checkpoint.Record
	var runWideNextSeq int64
	for rows.Next() {
		var rec checkpoint.Record
		var phase string
		var payload []byte
		if err := rows.Scan(&rec.SubRunID, &rec.StepID, &phase, &rec.Seq, &payload, &rec.RecordedAt); err != nil {
			return nil, fmt.Errorf("store: scanning checkpoint: %w", err)
		}
		rec.RunID = runID
		rec.Phase = checkpoint.Phase(phase)
		if payload != nil {
			rec.Payload = json.RawMessage(payload)
		}
		if rec.Seq >= runWideNextSeq {
			runWideNextSeq = rec.Seq + 1
		}
		if rec.SubRunID != subRunID {
			continue
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading checkpoints for run %s: %w", runID, err)
	}
	return checkpoint.NewSubRunState(runID, subRunID, records, runWideNextSeq), nil
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
