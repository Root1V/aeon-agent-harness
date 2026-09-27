package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A2ADelegations is the ledger of delegations the egress proxy let through (A2A-002).
//
// One table doing attribution, cost accounting and the fan-out count at once. Three tables would be
// three chances to disagree about whether a delegation happened, and the fan-out limit depends on the
// same rows the audit trail does — splitting them would let a run's traffic be under-counted while
// looking fully recorded.
type A2ADelegations struct {
	pool *pgxpool.Pool
}

// A2ADelegations returns a handle for A2A-002's delegation ledger.
func (s *Store) A2ADelegations() *A2ADelegations {
	return &A2ADelegations{pool: s.pool}
}

// Delegation is one proxied call to a remote agent.
type Delegation struct {
	ID               int64  `json:"id"`
	RunID            string `json:"run_id,omitempty"`
	StepID           string `json:"step_id,omitempty"`
	AgentManifestRef string `json:"agent_manifest_ref"`
	RemoteAgentID    string `json:"remote_agent_id"`
	RPCMethod        string `json:"rpc_method"`
	RemoteTaskID     string `json:"remote_task_id,omitempty"`
	// TaskState is what the remote reported, verbatim. Never normalized into our own vocabulary: an
	// unknown state must stay recognisable as the string the remote actually sent, or the record would
	// claim we understood something we did not.
	TaskState string `json:"task_state,omitempty"`
	// Terminal is OUR classification of TaskState, and it is a *bool because there are three answers:
	// terminal, not terminal, and not classified at all (a denial never reached a remote, so it has no
	// state to classify). A plain bool would record every denial as "not terminal", which reads as a
	// delegation still running.
	Terminal *bool `json:"terminal,omitempty"`
	// HopDepth is how many delegations deep this call is. Nil means the caller declared none — unknown
	// depth, not depth zero, and the difference matters because depth is Synaptum's to enforce.
	HopDepth     *int       `json:"hop_depth,omitempty"`
	DeniedReason string     `json:"denied_reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// ErrFanOutExceeded is returned when a run already has as many delegations in flight as it is allowed.
type fanOutError struct {
	RunID    string
	InFlight int
	Max      int
}

func (e *fanOutError) Error() string {
	return fmt.Sprintf("store: run %s already has %d delegation(s) in flight, limit is %d", e.RunID, e.InFlight, e.Max)
}

// FanOutExceeded reports whether err is a fan-out refusal, and the counts behind it.
func FanOutExceeded(err error) (inFlight, max int, ok bool) {
	var fe *fanOutError
	if errors.As(err, &fe) {
		return fe.InFlight, fe.Max, true
	}
	return 0, 0, false
}

// OpenOptions is what Open needs to admit or refuse a delegation.
type OpenOptions struct {
	Delegation Delegation
	// MaxInFlight is the width limit. Zero or less means unlimited, which is a deployment's choice and
	// not a default we pick: a limit nobody configured should not silently become 1.
	MaxInFlight int
	// StaleAfter bounds which open rows still count as in flight.
	//
	// THIS IS THE HONEST COST of enforcing width durably rather than in process. A gateway that dies
	// mid-delegation leaves its row open forever, and without a bound the run's budget would be
	// permanently spent by a call nobody is waiting for. With the bound, a delegation that legitimately
	// runs longer than StaleAfter stops being counted while it is still running — so the limit is
	// "concurrent delegations started within StaleAfter", not "concurrent delegations". Said here
	// because the difference only shows up under a slow remote, which is when it matters.
	StaleAfter time.Duration
}

// Open admits a delegation and records it as in flight, or refuses it on the fan-out limit.
//
// The count and the insert share one transaction holding a per-run advisory lock, which is the same
// device the checkpointer uses for seq. Without it, fifty simultaneous delegations would all read a
// count of zero and all be admitted — and fifty at once is precisely the case Synaptum raised on
// 2026-09-20 as the one nothing stops today.
//
// In flight is counted in POSTGRES and not in this process on purpose. An in-process counter is
// bypassed by the next gateway replica, and a limit that a second replica ignores is the comfortable
// kind of wrong: it reports a bound it does not have.
func (d *A2ADelegations) Open(ctx context.Context, opts OpenOptions) (*Delegation, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin delegation: %w", err)
	}
	defer tx.Rollback(context.Background())

	rec := opts.Delegation
	// Only a delegation that names a run can be counted, so only one can be limited. A call with no
	// run_id is still recorded and still policy-checked — it simply has no width budget to belong to,
	// and pretending otherwise would put every run-less caller in one shared bucket.
	if opts.MaxInFlight > 0 && rec.RunID != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "a2a-fanout:"+rec.RunID); err != nil {
			return nil, fmt.Errorf("store: locking run %s for fan-out: %w", rec.RunID, err)
		}
		staleAfter := opts.StaleAfter
		if staleAfter <= 0 {
			staleAfter = time.Hour
		}
		var inFlight int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM a2a_delegations
			   WHERE run_id = $1 AND completed_at IS NULL AND created_at > now() - $2::interval`,
			rec.RunID, fmt.Sprintf("%d seconds", int(staleAfter.Seconds())),
		).Scan(&inFlight); err != nil {
			return nil, fmt.Errorf("store: counting in-flight delegations for run %s: %w", rec.RunID, err)
		}
		if inFlight >= opts.MaxInFlight {
			return nil, &fanOutError{RunID: rec.RunID, InFlight: inFlight, Max: opts.MaxInFlight}
		}
	}

	var out Delegation
	err = tx.QueryRow(ctx,
		`INSERT INTO a2a_delegations (run_id, step_id, agent_manifest_ref, remote_agent_id, rpc_method, hop_depth)
		 VALUES (NULLIF($1, ''), NULLIF($2, ''), $3, $4, $5, $6)
		 RETURNING id, created_at`,
		rec.RunID, rec.StepID, rec.AgentManifestRef, rec.RemoteAgentID, rec.RPCMethod, rec.HopDepth,
	).Scan(&out.ID, &out.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("store: opening delegation to %q: %w", rec.RemoteAgentID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit delegation: %w", err)
	}

	out.RunID, out.StepID = rec.RunID, rec.StepID
	out.AgentManifestRef, out.RemoteAgentID = rec.AgentManifestRef, rec.RemoteAgentID
	out.RPCMethod, out.HopDepth = rec.RPCMethod, rec.HopDepth
	return &out, nil
}

// Observe records what the remote reported WITHOUT closing the row.
//
// This is what makes the width limit mean anything. A delegation is outstanding until the remote task
// reaches a terminal state — `message/send` typically returns a task in `working`, so closing the row at
// that point would leave in-flight permanently near zero and the fan-out limit would never fire. Fifty
// simultaneous delegations are fifty tasks still working, which is fifty open rows.
//
// So `completed_at` here means "the remote task finished", not "the HTTP request finished". The two are
// different and only one of them bounds blast radius.
func (d *A2ADelegations) Observe(ctx context.Context, id int64, remoteTaskID, taskState string, terminal *bool) error {
	_, err := d.pool.Exec(ctx,
		`UPDATE a2a_delegations
		    SET remote_task_id = COALESCE(NULLIF($2, ''), remote_task_id),
		        task_state = NULLIF($3, ''), terminal = $4
		  WHERE id = $1`,
		id, remoteTaskID, taskState, terminal)
	if err != nil {
		return fmt.Errorf("store: observing delegation %d: %w", id, err)
	}
	return nil
}

// Close records the remote's state and stops the row counting toward the run's width.
//
// terminal is a *bool for the same reason the column is nullable: a response whose state we could not read
// at all is not a non-terminal one. Called when the remote task has finished, and also when the call
// FAILED IN TRANSPORT — the delegation is over either way, and leaving the row open would spend the run's
// width budget on something nobody is waiting for.
func (d *A2ADelegations) Close(ctx context.Context, id int64, remoteTaskID, taskState string, terminal *bool) error {
	_, err := d.pool.Exec(ctx,
		`UPDATE a2a_delegations
		    SET remote_task_id = COALESCE(NULLIF($2, ''), remote_task_id),
		        task_state = NULLIF($3, ''), terminal = $4, completed_at = now()
		  WHERE id = $1`,
		id, remoteTaskID, taskState, terminal)
	if err != nil {
		return fmt.Errorf("store: closing delegation %d: %w", id, err)
	}
	return nil
}

// CloseByTaskID closes the open delegation that started a remote task, identified by that task.
//
// This is how a later poll frees the run's width: `tasks/get` is a different HTTP request, so it cannot
// close a row by id — it only knows which TASK finished. Without this path a run's width would be spent
// by tasks that completed and were only ever observed through a poll, which is the normal way an
// asynchronous delegation ends.
//
// Scoped to rows that are still open, so a repeated poll after completion is a harmless no-op rather than
// a second close moving completed_at forward.
func (d *A2ADelegations) CloseByTaskID(ctx context.Context, runID, remoteTaskID, taskState string, terminal *bool) (int64, error) {
	if remoteTaskID == "" {
		return 0, nil
	}
	tag, err := d.pool.Exec(ctx,
		`UPDATE a2a_delegations
		    SET task_state = NULLIF($3, ''), terminal = $4, completed_at = now()
		  WHERE remote_task_id = $2 AND completed_at IS NULL
		    AND (run_id = NULLIF($1, '') OR ($1 = '' AND run_id IS NULL))`,
		runID, remoteTaskID, taskState, terminal)
	if err != nil {
		return 0, fmt.Errorf("store: closing delegation for task %q: %w", remoteTaskID, err)
	}
	return tag.RowsAffected(), nil
}

// RecordDenial writes a delegation that never happened.
//
// Recorded rather than only refused, because "an agent tried to delegate somewhere it may not" is the
// event worth having: a refusal that leaves no trace is indistinguishable from an agent that never
// tried. Closed at insert — a denial is not in flight and must not consume the run's width.
func (d *A2ADelegations) RecordDenial(ctx context.Context, rec Delegation, reason string) error {
	_, err := d.pool.Exec(ctx,
		`INSERT INTO a2a_delegations
		   (run_id, step_id, agent_manifest_ref, remote_agent_id, rpc_method, hop_depth, denied_reason, completed_at)
		 VALUES (NULLIF($1, ''), NULLIF($2, ''), $3, $4, $5, $6, $7, now())`,
		rec.RunID, rec.StepID, rec.AgentManifestRef, rec.RemoteAgentID, rec.RPCMethod, rec.HopDepth, reason)
	if err != nil {
		return fmt.Errorf("store: recording denied delegation to %q: %w", rec.RemoteAgentID, err)
	}
	return nil
}

// ForRun returns a run's delegations in the order they were opened.
func (d *A2ADelegations) ForRun(ctx context.Context, runID string) ([]Delegation, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT id, COALESCE(run_id, ''), COALESCE(step_id, ''), agent_manifest_ref, remote_agent_id,
		        rpc_method, COALESCE(remote_task_id, ''), COALESCE(task_state, ''), terminal, hop_depth,
		        COALESCE(denied_reason, ''), created_at, completed_at
		   FROM a2a_delegations WHERE run_id = $1 ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: listing delegations for run %s: %w", runID, err)
	}
	defer rows.Close()

	out := []Delegation{}
	for rows.Next() {
		var rec Delegation
		if err := rows.Scan(&rec.ID, &rec.RunID, &rec.StepID, &rec.AgentManifestRef, &rec.RemoteAgentID,
			&rec.RPCMethod, &rec.RemoteTaskID, &rec.TaskState, &rec.Terminal, &rec.HopDepth,
			&rec.DeniedReason, &rec.CreatedAt, &rec.CompletedAt); err != nil {
			return nil, fmt.Errorf("store: scanning delegation: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
