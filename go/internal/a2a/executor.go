package a2a

import (
	"context"
	"fmt"
	"time"

	sdka2a "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"

	"github.com/aeon-ai/aeon/go/internal/runcontroller"
)

// defaultPollInterval is how often Execute checks a run's real status while waiting for it to
// reach a terminal state.
const defaultPollInterval = 250 * time.Millisecond

// AeonAgentExecutor bridges the A2A task lifecycle to a real Aeon run via the Run Controller
// (RUN-001) — the same Temporal-backed graph execution the REST /runs API already exposes,
// reached now through message/send instead of a native Aeon HTTP call. Graph is fixed at
// construction time: an incoming A2A message's content isn't yet translated into a graph spec of
// its own (see backlog.md) — every task this executor runs executes the same graph.
type AeonAgentExecutor struct {
	Controller *runcontroller.Controller
	Graph      map[string]any
	// PollEvery overrides defaultPollInterval — exposed for tests to poll faster than a real
	// operator would ever need to.
	PollEvery time.Duration
}

var _ a2asrv.AgentExecutor = (*AeonAgentExecutor)(nil)

// Execute starts a real run named after the task (so Cancel can later address the very same
// Temporal workflow deterministically) and reports the task's progress for real: an immediate
// "working" event, then the run's actual terminal outcome — never a synthetic "completed" that
// doesn't reflect what the underlying Aeon run actually did.
func (e *AeonAgentExecutor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	runID := string(reqCtx.TaskID)

	if err := q.Write(ctx, sdka2a.NewStatusUpdateEvent(reqCtx, sdka2a.TaskStateWorking, nil)); err != nil {
		return fmt.Errorf("a2a: write working event: %w", err)
	}

	if _, err := e.Controller.Start(ctx, runID, e.Graph, nil); err != nil {
		return e.writeTerminal(ctx, reqCtx, q, sdka2a.TaskStateFailed, "starting the run: "+err.Error())
	}

	workflowID := workflowIDFor(runID)
	interval := e.PollEvery
	if interval == 0 {
		interval = defaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Per the SDK's own AgentExecutor contract: a Cancel call that writes a canceled
			// event to this same task cancels this context — this is the normal cancellation
			// path, not a failure, so no terminal event is written here (Cancel already wrote
			// one).
			return ctx.Err()
		case <-ticker.C:
			status, err := e.Controller.Status(ctx, workflowID)
			if err != nil {
				return e.writeTerminal(ctx, reqCtx, q, sdka2a.TaskStateFailed, "checking run status: "+err.Error())
			}
			switch status.Status {
			case "SUCCEEDED":
				return e.writeTerminal(ctx, reqCtx, q, sdka2a.TaskStateCompleted, "")
			case "FAILED":
				return e.writeTerminal(ctx, reqCtx, q, sdka2a.TaskStateFailed, "the underlying Aeon run failed")
			case "CANCELLED":
				return e.writeTerminal(ctx, reqCtx, q, sdka2a.TaskStateCanceled, "the underlying Aeon run was canceled")
			}
			// RUNNING / PAUSED / PAUSED_FOR_APPROVAL: already reported as "working"; keep polling.
		}
	}
}

// Cancel requests real cancellation of the underlying Aeon run (not just an A2A-level status
// flip) and writes the canceled event itself — see the AgentExecutor doc comment on Cancel: doing
// so cancels Execute's context if it's still in flight, which is what stops Execute's poll loop
// from also racing to write its own terminal event.
func (e *AeonAgentExecutor) Cancel(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	workflowID := workflowIDFor(string(reqCtx.TaskID))
	if err := e.Controller.Cancel(ctx, workflowID); err != nil {
		return fmt.Errorf("a2a: cancel: %w", err)
	}
	event := sdka2a.NewStatusUpdateEvent(reqCtx, sdka2a.TaskStateCanceled, nil)
	event.Final = true
	return q.Write(ctx, event)
}

func (e *AeonAgentExecutor) writeTerminal(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue, state sdka2a.TaskState, detail string) error {
	var msg *sdka2a.Message
	if detail != "" {
		msg = sdka2a.NewMessageForTask(sdka2a.MessageRoleAgent, reqCtx, sdka2a.TextPart{Text: detail})
	}
	event := sdka2a.NewStatusUpdateEvent(reqCtx, state, msg)
	event.Final = true
	return q.Write(ctx, event)
}

// workflowIDFor mirrors runcontroller.Controller.Start's own internal workflow ID convention so
// Cancel/Status can address a run they didn't start themselves, deriving the same ID from just
// the task/run id.
func workflowIDFor(runID string) string {
	return "graph-run-" + runID
}
