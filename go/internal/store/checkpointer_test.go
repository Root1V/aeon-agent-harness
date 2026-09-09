package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/aeon-ai/aeon/go/internal/checkpoint"
)

func newTestCheckpointer(t *testing.T) *Checkpointer {
	t.Helper()
	return newTestStore(t).Checkpointer()
}

func newCheckpointRunID(label string) string {
	return fmt.Sprintf("cp-%s-%d", label, time.Now().UnixNano())
}

func mustAppend(t *testing.T, cp *Checkpointer, entry checkpoint.Entry) checkpoint.AppendResult {
	t.Helper()
	res, err := cp.Append(context.Background(), entry)
	if err != nil {
		t.Fatalf("append %s/%s: %v", entry.StepID, entry.Phase, err)
	}
	return res
}

// TestCheckpointerDeduplicatesByStepIdentity is INT-009's acceptance test: the durability seam
// agreed with Synaptum, against real Postgres and — for the case that matters most — a real
// Temporal server retrying a real Activity.
func TestCheckpointerDeduplicatesByStepIdentity(t *testing.T) {
	cp := newTestCheckpointer(t)
	ctx := context.Background()

	t.Run("appending the same identity repeatedly is a no-op, never an error", func(t *testing.T) {
		runID := newCheckpointRunID("dedupe")
		entry := checkpoint.Entry{RunID: runID, StepID: "s1", Phase: checkpoint.PhaseCompleted, Payload: json.RawMessage(`{"wrote":"a.txt"}`)}

		first := mustAppend(t, cp, entry)
		if first.Duplicate {
			t.Fatal("the first append reported Duplicate")
		}

		for i := 0; i < 3; i++ {
			again := mustAppend(t, cp, entry)
			if !again.Duplicate {
				t.Fatalf("repeat %d did not report Duplicate — the caller cannot tell a retry from a first attempt, so this is what protects it", i)
			}
			if again.Seq != first.Seq {
				t.Fatalf("repeat %d returned seq %d, want the existing %d", i, again.Seq, first.Seq)
			}
			if again.PayloadDiverged {
				t.Fatalf("repeat %d reported divergence for an identical payload", i)
			}
		}

		state, err := cp.Load(ctx, runID)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(state.Records()) != 1 {
			t.Fatalf("journal has %d records after 4 appends of one identity, want 1", len(state.Records()))
		}
	})

	t.Run("the same step in the other phase is a different entry", func(t *testing.T) {
		// phase is part of the identity, not decoration: attempted and completed for one step are
		// two journal entries, or the crash-in-the-middle case cannot be represented at all.
		runID := newCheckpointRunID("phases")
		base := checkpoint.Entry{RunID: runID, StepID: "s1"}

		attempted := base
		attempted.Phase = checkpoint.PhaseAttempted
		completed := base
		completed.Phase = checkpoint.PhaseCompleted
		completed.Payload = json.RawMessage(`{"ok":true}`)

		a := mustAppend(t, cp, attempted)
		c := mustAppend(t, cp, completed)
		if c.Duplicate || a.Seq == c.Seq {
			t.Fatalf("the two phases collapsed into one entry: attempted=%+v completed=%+v", a, c)
		}

		state, err := cp.Load(ctx, runID)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if state.Attempted("s1") {
			t.Error("a step with both phases must report completed, not attempted")
		}
		if _, ok := state.Completed("s1"); !ok {
			t.Error("expected s1 to report completed")
		}
		if state.NextSeq() != 2 {
			t.Errorf("NextSeq() = %d, want 2", state.NextSeq())
		}
	})

	t.Run("a duplicate carrying a different result keeps the first and reports the divergence", func(t *testing.T) {
		// First write wins, because that is the one later readers may already have acted on. But
		// two attempts of one step producing different results is real non-determinism, and a
		// journal that silently swallows it is worse than no journal.
		runID := newCheckpointRunID("diverged")
		entry := checkpoint.Entry{RunID: runID, StepID: "s1", Phase: checkpoint.PhaseCompleted, Payload: json.RawMessage(`{"answer":41}`)}
		mustAppend(t, cp, entry)

		entry.Payload = json.RawMessage(`{"answer":42}`)
		res := mustAppend(t, cp, entry)
		if !res.Duplicate || !res.PayloadDiverged {
			t.Fatalf("append with a different payload = %+v, want Duplicate and PayloadDiverged", res)
		}

		state, err := cp.Load(ctx, runID)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		got, _ := state.Completed("s1")
		if string(got.Payload) != `{"answer": 41}` && string(got.Payload) != `{"answer":41}` {
			t.Errorf("stored payload = %s, want the first write to have won", got.Payload)
		}
	})

	t.Run("a retry that reorders the payload's keys is not a divergence", func(t *testing.T) {
		// The caller across this seam is Python; dict ordering between two attempts is not
		// something we get to assume. Comparing jsonb semantically is what keeps this from being a
		// false alarm on every retry.
		runID := newCheckpointRunID("reordered")
		entry := checkpoint.Entry{RunID: runID, StepID: "s1", Phase: checkpoint.PhaseCompleted, Payload: json.RawMessage(`{"a":1,"b":2}`)}
		mustAppend(t, cp, entry)

		entry.Payload = json.RawMessage(`{"b":2,"a":1}`)
		res := mustAppend(t, cp, entry)
		if !res.Duplicate {
			t.Fatal("expected a duplicate")
		}
		if res.PayloadDiverged {
			t.Error("re-serialising the same object with its keys in a different order was reported as divergence")
		}
	})

	t.Run("concurrent appends for one run get contiguous sequence numbers", func(t *testing.T) {
		// Parallel graph nodes append to the same run at the same time. Contiguity is what makes
		// NextSeq mean "where the journal continues" rather than "some number past the last one".
		runID := newCheckpointRunID("parallel")
		const n = 12

		var wg sync.WaitGroup
		seqs := make([]int64, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				res, err := cp.Append(ctx, checkpoint.Entry{
					RunID: runID, StepID: fmt.Sprintf("s%d", i), Phase: checkpoint.PhaseCompleted,
				})
				seqs[i], errs[i] = res.Seq, err
			}(i)
		}
		wg.Wait()

		seen := map[int64]bool{}
		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent append %d: %v", i, err)
			}
			if seen[seqs[i]] {
				t.Fatalf("seq %d was handed out twice", seqs[i])
			}
			seen[seqs[i]] = true
		}
		for want := int64(0); want < n; want++ {
			if !seen[want] {
				t.Fatalf("seq %d missing — the journal has a gap", want)
			}
		}

		state, err := cp.Load(ctx, runID)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if state.NextSeq() != n {
			t.Errorf("NextSeq() = %d, want %d", state.NextSeq(), n)
		}
	})

	t.Run("an Activity retried by real Temporal journals the step exactly once", func(t *testing.T) {
		testCheckpointerUnderTemporalRetry(t, cp)
	})
}

// checkpointActivities is the real Activity used by the acceptance test above: it journals the
// step's result and then fails, the way a worker dying after its effect has landed but before
// Temporal recorded the completion does. Temporal then retries it — at-least-once execution, which
// is the condition the seam's idempotency exists for.
type checkpointActivities struct {
	cp *Checkpointer

	mu      sync.Mutex
	results []checkpoint.AppendResult
}

const failUntilAttempt = 3

func (a *checkpointActivities) AppendThenFail(ctx context.Context, entry checkpoint.Entry) error {
	res, err := a.cp.Append(ctx, entry)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.results = append(a.results, res)
	a.mu.Unlock()

	if activity.GetInfo(ctx).Attempt < failUntilAttempt {
		return errors.New("simulated worker death after the effect was journalled")
	}
	return nil
}

func (a *checkpointActivities) snapshot() []checkpoint.AppendResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]checkpoint.AppendResult(nil), a.results...)
}

// checkpointRetryWorkflow runs the activity above under a retry policy that will exercise every
// attempt. It stays deterministic: all it does is call an Activity.
func checkpointRetryWorkflow(ctx workflow.Context, entry checkpoint.Entry) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 20 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    100 * time.Millisecond,
			BackoffCoefficient: 1.0,
			MaximumAttempts:    failUntilAttempt,
		},
	})
	return workflow.ExecuteActivity(ctx, "AppendThenFail", entry).Get(ctx, nil)
}

func testCheckpointerUnderTemporalRetry(t *testing.T, cp *Checkpointer) {
	t.Helper()
	addr := os.Getenv("AEON_TEST_TEMPORAL_ADDRESS")
	if addr == "" {
		t.Skip("AEON_TEST_TEMPORAL_ADDRESS not set — skipping the Temporal half of INT-009 (see make test-go-integration)")
	}
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("connecting to Temporal at %s: %v", addr, err)
	}
	defer c.Close()

	runID := newCheckpointRunID("temporal")
	taskQueue := "checkpoint-test-" + runID
	acts := &checkpointActivities{cp: cp}

	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(checkpointRetryWorkflow)
	w.RegisterActivityWithOptions(acts.AppendThenFail, activity.RegisterOptions{Name: "AppendThenFail"})
	if err := w.Start(); err != nil {
		t.Fatalf("starting worker: %v", err)
	}
	defer w.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	entry := checkpoint.Entry{
		RunID: runID, StepID: "charge-card", Phase: checkpoint.PhaseCompleted,
		Payload: json.RawMessage(`{"charged":true}`),
	}
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: runID, TaskQueue: taskQueue}, checkpointRetryWorkflow, entry)
	if err != nil {
		t.Fatalf("starting workflow: %v", err)
	}
	if err := run.Get(ctx, nil); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	results := acts.snapshot()
	if len(results) != failUntilAttempt {
		t.Fatalf("the activity ran %d times, want %d — the retry this test depends on did not happen", len(results), failUntilAttempt)
	}
	if results[0].Duplicate {
		t.Error("the first attempt reported Duplicate")
	}
	for i, res := range results[1:] {
		if !res.Duplicate {
			t.Errorf("retry %d wrote a second entry instead of reporting Duplicate", i+2)
		}
		if res.Seq != results[0].Seq {
			t.Errorf("retry %d got seq %d, want the first attempt's %d", i+2, res.Seq, results[0].Seq)
		}
	}

	state, err := cp.Load(ctx, runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.Records()) != 1 {
		t.Fatalf("the journal holds %d entries after %d real Activity attempts, want exactly 1", len(state.Records()), len(results))
	}
	if _, ok := state.Completed("charge-card"); !ok {
		t.Error("the step is not reported completed after the workflow succeeded")
	}
	t.Logf("real Temporal ran the Activity %d times; the journal holds %d entry", len(results), len(state.Records()))
}
