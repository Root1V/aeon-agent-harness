package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/checkpoint"
)

// VRT-AEON-004's acceptance test, with Veritium's criterion adopted verbatim.
//
// THE DEFECT, in their words: "que un nieto de una delegación perdiera registros sin error sería
// justo el tipo de fallo que no detectaríamos". With nested delegation two different sub-runs of one
// run legitimately use the same step_id — separate loops, each numbering its own steps — and the
// primary key did not include the sub-run. So the second sub-run's entry was a DUPLICATE of the
// first's: Append wrote nothing, returned Duplicate=true, and reported success. The journal then
// said a step had already run when it had not, and a resume would skip real work.
//
// AND WHY THEY ASKED FOR IT BEFORE ANYONE HIT IT: `TestCheckpointerDeduplicatesByStepIdentity` would
// have stayed green either way. It asserts that a duplicate does NOT create a second row, which is
// the opposite of what has to be distinguished here. A test suite can be entirely green over this.
//
// Real Postgres, through the real migration: the property is a primary key.
func TestTwoSubRunsOfOneRunKeepTheirOwnJournals(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cp := s.CheckpointerFor("default")

	runID := "delegating-run-" + randSuffix(t)
	const stepID = "step-1"
	// Two sibling delegations and a grandchild under one of them. The paths are opaque to the seam —
	// nothing parses them — so a grandchild is simply a longer string.
	paths := []string{"", "child-a", "child-b", "child-b/grandchild"}

	for _, path := range paths {
		payload, _ := json.Marshal(map[string]any{"who": path})
		res, err := cp.Append(ctx, checkpoint.Entry{
			RunID: runID, SubRunID: path, StepID: stepID,
			Phase: checkpoint.PhaseCompleted, Payload: payload,
		})
		if err != nil {
			t.Fatalf("Append for sub-run %q: %v", path, err)
		}
		// THE ASSERTION THE WHOLE FEATURE IS ABOUT. Before this change every path after the first
		// came back Duplicate=true with nothing written — a silent loss reported as success.
		if res.Duplicate {
			t.Fatalf("sub-run %q was treated as a duplicate of an earlier sub-run's %q: its record was "+
				"never written and Append said success", path, stepID)
		}
	}

	state, err := cp.Load(ctx, runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(state.Records()); got != len(paths) {
		t.Fatalf("the run's journal holds %d records, want %d — one sub-run's entry overwrote another's",
			got, len(paths))
	}

	// Each one reads back as ITS OWN, which is what makes the count above mean something: four rows
	// that all carried the same payload would satisfy a count and still be the defect.
	for _, path := range paths {
		rec, ok := state.Completed(path, stepID)
		if !ok {
			t.Fatalf("no completed record for sub-run %q step %q", path, stepID)
		}
		var payload map[string]any
		if err := json.Unmarshal(rec.Payload, &payload); err != nil {
			t.Fatalf("sub-run %q payload: %v", path, err)
		}
		if payload["who"] != path {
			t.Errorf("sub-run %q reads back the payload of %v — the journals are crossed", path, payload["who"])
		}
	}

	// And a genuine retry of ONE sub-run is still deduplicated. Without this the change could have
	// been "make the key unique enough to never collide", which would break the seam's first rule:
	// a caller under at-least-once execution cannot tell a retry from a first attempt.
	payload, _ := json.Marshal(map[string]any{"who": "child-b"})
	res, err := cp.Append(ctx, checkpoint.Entry{
		RunID: runID, SubRunID: "child-b", StepID: stepID,
		Phase: checkpoint.PhaseCompleted, Payload: payload,
	})
	if err != nil {
		t.Fatalf("re-Append: %v", err)
	}
	if !res.Duplicate {
		t.Fatal("a genuine retry of one sub-run's step was not deduplicated — idempotency by " +
			"(run, sub_run, step, phase) is the seam's first rule and this change must not have " +
			"traded it away")
	}
	if res.PayloadDiverged {
		t.Error("the identical retry was reported as a divergence")
	}

	// seq is RUN-WIDE: one ordering for the run and its delegations together, so NextSeq keeps
	// meaning "where does this run's journal continue".
	if want := int64(len(paths)); state.NextSeq() != want {
		t.Errorf("NextSeq() = %d, want %d — seq must be numbered over the run, not per sub-run, or two "+
			"entries share a position and the ordering stops being total", state.NextSeq(), want)
	}
}

// TestTheRootRunsSentinelIsNotTheClientsProblem is the second half of what Veritium asked for: the
// sentinel is in the schema, so a caller with no delegation omits the field entirely.
func TestTheRootRunsSentinelIsNotTheClientsProblem(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cp := s.CheckpointerFor("default")
	runID := "root-only-run-" + randSuffix(t)

	// No SubRunID set at all — the zero value, which is what an HttpCheckpointer that knows nothing
	// about delegation sends.
	if _, err := cp.Append(ctx, checkpoint.Entry{
		RunID: runID, StepID: "s1", Phase: checkpoint.PhaseAttempted,
	}); err != nil {
		t.Fatalf("Append with no sub-run: %v", err)
	}
	if _, err := cp.Append(ctx, checkpoint.Entry{
		RunID: runID, StepID: "s1", Phase: checkpoint.PhaseCompleted,
	}); err != nil {
		t.Fatalf("Append completed with no sub-run: %v", err)
	}

	state, err := cp.Load(ctx, runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// A NULLABLE column would have been worse than asking the client to choose: NULL is not equal to
	// NULL in a unique index, so every root entry would have been unique against itself and the
	// deduplication below would simply have stopped working. Asserted rather than argued.
	if _, ok := state.Completed("", "s1"); !ok {
		t.Fatal("the root run's completed record is not readable under the empty sub-run — the sentinel " +
			"the schema writes is not the one the reader asks with")
	}
	if state.Attempted("", "s1") {
		t.Error("a step with both phases reads as merely attempted")
	}
	if _, err := cp.Append(ctx, checkpoint.Entry{
		RunID: runID, StepID: "s1", Phase: checkpoint.PhaseCompleted,
	}); err != nil {
		t.Fatalf("re-Append: %v", err)
	} else if got := len(mustLoad(t, cp, runID).Records()); got != 2 {
		t.Fatalf("the run holds %d records after a duplicate, want 2 — with a NULL sentinel this is "+
			"where deduplication stops working", got)
	}
}

func mustLoad(t *testing.T, cp *Checkpointer, runID string) *checkpoint.RunState {
	t.Helper()
	state, err := cp.Load(context.Background(), runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return state
}
