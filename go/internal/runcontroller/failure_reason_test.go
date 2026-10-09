package runcontroller

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// reasonProbeWorkflow fails in the three shapes a real run fails in.
func reasonProbeWorkflow(ctx workflow.Context, mode string) error {
	switch mode {
	case "workflow-error":
		// What a budget stop looks like: the workflow itself refusing, with a type a consumer can
		// branch on.
		return temporal.NewNonRetryableApplicationError(
			"budget exceeded: tokens 3000 recorded, ceiling 3000", "BudgetExceeded", nil)
	case "activity-error":
		// The common case, and the one whose message is NOT at the top of the chain.
		return workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: 5 * time.Second,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
		}), reasonProbeActivity).Get(ctx, nil)
	case "sleep":
		return workflow.Sleep(ctx, 10*time.Minute)
	}
	return nil
}

func reasonProbeActivity(ctx context.Context) error {
	return temporal.NewNonRetryableApplicationError(
		"denied by policy: no bundle is loaded for this tenant", "PolicyDenied", nil)
}

// TestARunSaysWhyItFailed is OBS-010's acceptance test, against a real Temporal server.
//
// VERITIUM ASKED FOR IT WITHOUT ASKING. Closing VRT-AEON-001 they left three second-consumer notes
// and said they wanted nothing: "GET /runs/{id} no dice por qué falló un run. Como aeon no avisa,
// marcamos nuestra corrida fallida con un reconciliador que consulta el estado (FAILED / CANCELLED).
// Nos basta. Si algún día exponen el motivo, lo registraríamos." A consumer whose reconciliator can
// see THAT a run failed and not WHY has to open Temporal's own UI, which is the one place a platform
// consumer should not have to look.
//
// Real Temporal and not a fake client, because every fact this feature reports comes out of the
// close event and the shape of that event is the thing being relied on. A fake would assert the
// shape I believe it has — and the measurement that designed this test contradicted my first guess
// twice: an activity failure's useful message is one level DOWN the cause chain, and a termination's
// reason is not in the error `WorkflowRun.Get` returns at all (it answers the bare string
// "terminated"), which is why this reads the history.
func TestARunSaysWhyItFailed(t *testing.T) {
	addr := os.Getenv("AEON_TEST_TEMPORAL_ADDRESS")
	if addr == "" {
		t.Skip("AEON_TEST_TEMPORAL_ADDRESS not set — skipping Temporal integration test (see make test-go-integration)")
	}
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("connecting to Temporal at %s: %v", addr, err)
	}
	t.Cleanup(c.Close)

	queue := fmt.Sprintf("failure-reason-%d", time.Now().UnixNano())
	w := worker.New(c, queue, worker.Options{})
	w.RegisterWorkflow(reasonProbeWorkflow)
	w.RegisterActivity(reasonProbeActivity)
	if err := w.Start(); err != nil {
		t.Fatalf("starting the worker: %v", err)
	}
	t.Cleanup(w.Stop)

	ctrl := New(c, queue)
	ctx := context.Background()

	t.Run("a workflow error reports its message, type and that retrying is pointless", func(t *testing.T) {
		id := fmt.Sprintf("fr-wf-%d", time.Now().UnixNano())
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: queue}, reasonProbeWorkflow, "workflow-error")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		_ = run.Get(ctx, nil)

		status, err := ctrl.Status(ctx, id, DefaultTenant)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if status.Status != "FAILED" {
			t.Fatalf("status = %q, want FAILED", status.Status)
		}
		if status.Failure == nil {
			t.Fatal("the run failed and the status carries no failure — which is the whole defect: a " +
				"consumer can see THAT and not WHY")
		}
		f := status.Failure
		if f.Kind != "failed" {
			t.Errorf("kind = %q, want failed", f.Kind)
		}
		if f.Message != "budget exceeded: tokens 3000 recorded, ceiling 3000" {
			t.Errorf("message = %q, want the workflow's own text", f.Message)
		}
		if f.Type != "BudgetExceeded" {
			t.Errorf("type = %q, want BudgetExceeded — the vocabulary a consumer branches on without "+
				"parsing a message", f.Type)
		}
		// Three states: absent would mean "the failure said nothing about retrying", and this one did.
		if f.Retryable == nil {
			t.Fatal("retryable is absent for a failure that declared itself non-retryable")
		}
		if *f.Retryable {
			t.Error("retryable = true for a NonRetryableApplicationError — a worker reading this would " +
				"loop on a budget stop that can never be allowed again")
		}
		if f.Activity != "" {
			t.Errorf("activity = %q for a failure that came from the workflow itself", f.Activity)
		}
	})

	t.Run("an activity error reports the activity AND the message one level down", func(t *testing.T) {
		// THE SHAPE THAT CONTRADICTED MY FIRST GUESS. The outermost message of an activity failure is
		// the literal string "activity error"; the useful text is the cause's. Reporting the outer one
		// would have given every activity failure in the platform the same useless message, and the
		// test would have been green.
		id := fmt.Sprintf("fr-act-%d", time.Now().UnixNano())
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: queue}, reasonProbeWorkflow, "activity-error")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		_ = run.Get(ctx, nil)

		status, err := ctrl.Status(ctx, id, DefaultTenant)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if status.Failure == nil {
			t.Fatal("no failure reported for a run whose activity failed")
		}
		f := status.Failure
		if f.Message != "denied by policy: no bundle is loaded for this tenant" {
			t.Errorf("message = %q, want the ACTIVITY's text and not the %q wrapper",
				f.Message, "activity error")
		}
		if f.Activity != "reasonProbeActivity" {
			t.Errorf("activity = %q, want reasonProbeActivity — it sits at the outer level while the "+
				"message sits at the inner, so both have to be collected separately", f.Activity)
		}
		if f.Type != "PolicyDenied" {
			t.Errorf("type = %q, want PolicyDenied", f.Type)
		}
	})

	t.Run("a terminated run is distinguishable from one that failed, and says who and why", func(t *testing.T) {
		// THE DISTINCTION `status` LOSES. Both answer FAILED, and an operator killing a run is not the
		// same incident as a run raising an error. The reason is also the one field WorkflowRun.Get
		// cannot give — measured: it answers the bare string "terminated".
		id := fmt.Sprintf("fr-term-%d", time.Now().UnixNano())
		if _, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: queue}, reasonProbeWorkflow, "sleep"); err != nil {
			t.Fatalf("start: %v", err)
		}
		const reason = "operator stopped it: suspected data exfiltration"
		// Waits for the workflow to actually be running before terminating it, rather than sleeping a
		// fixed interval and hoping — the pattern that made A2A-002's cancel subtest flake twice.
		waitRunning(t, ctrl, ctx, id)
		if err := c.TerminateWorkflow(ctx, id, "", reason, nil); err != nil {
			t.Fatalf("terminate: %v", err)
		}

		status := waitClosed(t, ctrl, ctx, id)
		if status.Status != "FAILED" {
			t.Fatalf("status = %q, want FAILED (the enum has no TERMINATED, which is why kind exists)", status.Status)
		}
		if status.Failure == nil {
			t.Fatal("no failure reported for a terminated run")
		}
		if status.Failure.Kind != "terminated" {
			t.Fatalf("kind = %q, want terminated — without it this is indistinguishable from a workflow "+
				"error, and the next action is not the same", status.Failure.Kind)
		}
		if status.Failure.Message != reason {
			t.Errorf("message = %q, want the operator's reason %q", status.Failure.Message, reason)
		}
		if status.Failure.TerminatedBy == "" {
			t.Error("terminated_by is empty — Temporal's identity is the only trace of who did it")
		}
	})

	t.Run("a timeout is its own kind, and leaves retryable ABSENT", func(t *testing.T) {
		// TWO THINGS THIS SUBTEST EXISTS FOR, and the second one was missing until a negative control
		// said so. The first is the `timed_out` kind, the third of the three `status` merges into
		// FAILED. The second is the THIRD STATE of `retryable`: a timeout carries no application
		// failure info, so nothing said anything about retrying, and that has to read as ABSENT rather
		// than as false. I claimed that in the design and in the schema and did not test it — the
		// control that forced a constant `false` passed, because every case I had covered happened to
		// be non-retryable.
		id := fmt.Sprintf("fr-timeout-%d", time.Now().UnixNano())
		if _, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID: id, TaskQueue: queue,
			// Short on purpose: the run sleeps for ten minutes and this is what ends it.
			WorkflowExecutionTimeout: 2 * time.Second,
			RetryPolicy:              &temporal.RetryPolicy{MaximumAttempts: 1},
		}, reasonProbeWorkflow, "sleep"); err != nil {
			t.Fatalf("start: %v", err)
		}

		status := waitClosed(t, ctrl, ctx, id)
		if status.Failure == nil {
			t.Fatal("no failure reported for a run that timed out")
		}
		if status.Failure.Kind != "timed_out" {
			t.Fatalf("kind = %q, want timed_out — a run that ran out of time and one that raised an "+
				"error are different incidents and `status` calls both FAILED", status.Failure.Kind)
		}
		if status.Failure.Retryable != nil {
			t.Errorf("retryable = %v for a timeout, which carries no application info to say either way. "+
				"A present false here would be a fabricated claim — the same shape MDL-014 removed from "+
				"the token counters", *status.Failure.Retryable)
		}
		if status.Failure.Message == "" {
			t.Error("the timeout reports no message at all; Temporal gives none, so this one is ours " +
				"and it has to say WHICH timeout")
		}
	})

	t.Run("a healthy run carries no failure at all", func(t *testing.T) {
		// The half that makes the rest mean something: a `failure` that showed up on a run that
		// succeeded would be worse than none, and a status read on a RUNNING or SUCCEEDED run must not
		// pay for a history call either.
		id := fmt.Sprintf("fr-ok-%d", time.Now().UnixNano())
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: queue}, reasonProbeWorkflow, "fine")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if err := run.Get(ctx, nil); err != nil {
			t.Fatalf("the happy-path workflow failed: %v", err)
		}
		status, err := ctrl.Status(ctx, id, DefaultTenant)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if status.Status != "SUCCEEDED" {
			t.Fatalf("status = %q, want SUCCEEDED", status.Status)
		}
		if status.Failure != nil {
			t.Fatalf("a successful run reports a failure: %+v", status.Failure)
		}
	})
}

func waitRunning(t *testing.T, ctrl *Controller, ctx context.Context, id string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if s, err := ctrl.Status(ctx, id, DefaultTenant); err == nil && s.Status == "RUNNING" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run %s never reached RUNNING", id)
}

func waitClosed(t *testing.T, ctrl *Controller, ctx context.Context, id string) *Status {
	t.Helper()
	for i := 0; i < 100; i++ {
		s, err := ctrl.Status(ctx, id, DefaultTenant)
		if err == nil && s.Status != "RUNNING" && s.Status != "PENDING" {
			return s
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run %s never closed", id)
	return nil
}
