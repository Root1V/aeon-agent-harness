// Package runcontroller is RUN-001: start/cancel/pause/resume/status/stream for a run. Every
// operation except pause/resume/is_paused is a native Temporal client call (StartWorkflow,
// CancelWorkflow, DescribeWorkflowExecution) — the workflow itself (python/aeon_worker/workflows/
// graph_run.py) needs no code for those. Pause/resume are Temporal signals; is_paused is a query,
// both handled by GraphRunWorkflow.
package runcontroller

import (
	"context"
	"fmt"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

const (
	// DefaultTaskQueue matches aeon_worker.__main__'s AEON_TASK_QUEUE default.
	DefaultTaskQueue = "aeon-agent-run"
	// WorkflowType matches GraphRunWorkflow's class name — Temporal identifies workflow types by
	// this string when started from a client that doesn't import the Python class directly.
	WorkflowType = "GraphRunWorkflow"
)

// Controller wraps a Temporal client with the run-lifecycle operations RUN-001 promises.
type Controller struct {
	Client    client.Client
	TaskQueue string
}

// New returns a Controller. taskQueue defaults to DefaultTaskQueue when empty.
func New(c client.Client, taskQueue string) *Controller {
	if taskQueue == "" {
		taskQueue = DefaultTaskQueue
	}
	return &Controller{Client: c, TaskQueue: taskQueue}
}

// RunInfo identifies a started run both by our own run_id and Temporal's own identifiers.
type RunInfo struct {
	RunID         string `json:"run_id"`
	WorkflowID    string `json:"workflow_id"`
	TemporalRunID string `json:"temporal_run_id"`
}

// Start begins a new GraphRunWorkflow. runID becomes both the graph's run_id (threaded into every
// idempotency key, docs/adr/0001) and part of the Temporal workflow ID. budgets is optional
// (RUN-003) — pass nil for no limits — and is shaped like {"max_tool_calls": int,
// "max_depth": int, "deadline_seconds": int}; see graph_run.py's _budget_policy_from_request.
func (c *Controller) Start(ctx context.Context, runID string, graph map[string]any, budgets map[string]any) (*RunInfo, error) {
	workflowID := "graph-run-" + runID
	input := map[string]any{"run_id": runID, "graph": graph}
	if budgets != nil {
		input["budgets"] = budgets
	}
	run, err := c.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: c.TaskQueue,
	}, WorkflowType, input)
	if err != nil {
		return nil, fmt.Errorf("runcontroller: start: %w", err)
	}
	return &RunInfo{RunID: runID, WorkflowID: workflowID, TemporalRunID: run.GetRunID()}, nil
}

// Cancel requests cancellation of a run. The workflow does not catch cancellation, so it ends in
// Temporal's CANCELED status — reflected as our "CANCELLED" by Status.
func (c *Controller) Cancel(ctx context.Context, workflowID string) error {
	if err := c.Client.CancelWorkflow(ctx, workflowID, ""); err != nil {
		return fmt.Errorf("runcontroller: cancel: %w", err)
	}
	return nil
}

// Pause signals a run to stop before its next GraphNode. Already-in-flight Activity calls still
// complete; execute_graph checks the pause gate between nodes, not mid-Activity (graph.py).
func (c *Controller) Pause(ctx context.Context, workflowID string) error {
	if err := c.Client.SignalWorkflow(ctx, workflowID, "", "pause", nil); err != nil {
		return fmt.Errorf("runcontroller: pause: %w", err)
	}
	return nil
}

// Resume signals a paused run to proceed.
func (c *Controller) Resume(ctx context.Context, workflowID string) error {
	if err := c.Client.SignalWorkflow(ctx, workflowID, "", "resume", nil); err != nil {
		return fmt.Errorf("runcontroller: resume: %w", err)
	}
	return nil
}

// Status is RunState's status field (proto/schemas/run_state.schema.json), derived from Temporal's
// own execution status plus (while RUNNING) the workflow's own is_paused query.
type Status struct {
	WorkflowID      string         `json:"workflow_id"`
	Status          string         `json:"status"`
	Paused          bool           `json:"paused"`
	BudgetsConsumed map[string]any `json:"budgets_consumed,omitempty"`
}

// Status fetches a run's current status. budgets_consumed (RUN-003) is queried regardless of
// terminal-ness — Temporal answers queries against a closed workflow by replaying its history, so
// this still reports accurate counts after e.g. a budget-triggered failure.
func (c *Controller) Status(ctx context.Context, workflowID string) (*Status, error) {
	desc, err := c.Client.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		return nil, fmt.Errorf("runcontroller: describe: %w", err)
	}
	temporalStatus := desc.WorkflowExecutionInfo.GetStatus()

	paused := false
	if temporalStatus == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		val, err := c.Client.QueryWorkflow(ctx, workflowID, "", "is_paused")
		if err == nil {
			_ = val.Get(&paused)
		}
	}

	var budgetsConsumed map[string]any
	if val, err := c.Client.QueryWorkflow(ctx, workflowID, "", "budgets_consumed"); err == nil {
		_ = val.Get(&budgetsConsumed)
	}

	return &Status{
		WorkflowID:      workflowID,
		Status:          mapStatus(temporalStatus, paused),
		Paused:          paused,
		BudgetsConsumed: budgetsConsumed,
	}, nil
}

// mapStatus translates Temporal's execution status into RunState's status enum
// (proto/schemas/run_state.schema.json).
func mapStatus(s enumspb.WorkflowExecutionStatus, paused bool) string {
	switch s {
	case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:
		if paused {
			return "PAUSED"
		}
		return "RUNNING"
	case enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		return "SUCCEEDED"
	case enumspb.WORKFLOW_EXECUTION_STATUS_FAILED, enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT, enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		return "FAILED"
	case enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:
		return "CANCELLED"
	default:
		return "PENDING"
	}
}
