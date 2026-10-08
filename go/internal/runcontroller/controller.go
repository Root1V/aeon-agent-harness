// Package runcontroller is RUN-001: start/cancel/pause/resume/status/stream for a run. Every
// operation except pause/resume/is_paused is a native Temporal client call (StartWorkflow,
// CancelWorkflow, DescribeWorkflowExecution) — the workflow itself (python/aeon_worker/workflows/
// graph_run.py) needs no code for those. Pause/resume are Temporal signals; is_paused is a query,
// both handled by GraphRunWorkflow.
package runcontroller

import (
	"context"
	"fmt"
	"log"
	"strings"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/aeon-ai/aeon/go/internal/store"
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
	// Ledger is where a run's real cost and token count come from (MDL-018). Optional: without it the
	// status omits them rather than reporting zeros, which is the defect it exists to remove.
	// VRT-AEON-005: the store. A run's spend is a TENANT's spend, so the handle is derived from the
	// tenant the caller passes to Status rather than fixed when the Controller is built.
	Ledger *store.Store
}

// WorkflowIDPrefix is what a run id becomes as a Temporal workflow id. Named once here because
// MDL-018 needs to go the other way — the ledger is keyed by the run id the caller chose — and two
// hand-written copies of a prefix is how the status endpoint ends up looking up the wrong run.
// WorkflowIDPrefix is exported because the HTTP layer builds the same id (go/internal/api).
const WorkflowIDPrefix = "graph-run-"

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
func (c *Controller) Start(ctx context.Context, runID string, graph map[string]any, budgets map[string]any, agentManifestRef string) (*RunInfo, error) {
	workflowID := WorkflowIDPrefix + runID
	input := map[string]any{"run_id": runID, "graph": graph}
	// TOOL-004: the principal the Tool Gateway evaluates policy against. It already arrived at the
	// API (A5 uses it to refuse a quarantined agent) and simply never reached the worker, so every
	// tool call the worker made was unattributable — and a per-agent policy cannot govern a call
	// that carries no agent.
	if agentManifestRef != "" {
		input["agent_manifest_ref"] = agentManifestRef
	}
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

// PendingApproval fetches the run's pending_approval query (RUN-005), or nil if nothing is
// currently pending. Shaped like RunState's pending_approval field (proto/schemas/run_state.schema.json).
func (c *Controller) PendingApproval(ctx context.Context, workflowID string) (map[string]any, error) {
	val, err := c.Client.QueryWorkflow(ctx, workflowID, "", "pending_approval")
	if err != nil {
		return nil, fmt.Errorf("runcontroller: pending_approval: %w", err)
	}
	var pending map[string]any
	if err := val.Get(&pending); err != nil {
		return nil, fmt.Errorf("runcontroller: pending_approval: decoding: %w", err)
	}
	return pending, nil
}

// Approve signals approval of toolCallHash. It first fetches the currently pending approval to
// find its approval_id — a caller only ever needs to name the hash it saw in Status/
// PendingApproval, not track approval_ids itself. The workflow (graph_run.py's _await_approval)
// is what actually enforces that toolCallHash matches the parameters about to execute
// (RUN-005's parameter binding) — passing it through unmodified here, rather than re-deriving it
// from the pending approval, is what lets a caller approve the WRONG hash and be denied.
func (c *Controller) Approve(ctx context.Context, workflowID, toolCallHash string) (ApprovalDecision, error) {
	return c.sendApprovalDecision(ctx, workflowID, "approve", toolCallHash, true)
}

// Reject signals rejection of toolCallHash.
func (c *Controller) Reject(ctx context.Context, workflowID, toolCallHash string) (ApprovalDecision, error) {
	return c.sendApprovalDecision(ctx, workflowID, "reject", toolCallHash, false)
}

// ApprovalDecision is what a person decided, returned so the caller can journal it (INT-011).
//
// Returned rather than journalled in here because this type's job is Temporal and nothing else; the
// journal write belongs to the harness surface that received the person's request. What this type does
// owe the caller is the IDENTITY of the decision — approval_id, node_id — which only it sees, because
// it is the one that queried the pending approval to find them.
type ApprovalDecision struct {
	ApprovalID   string `json:"approval_id"`
	NodeID       string `json:"node_id,omitempty"`
	ToolCallHash string `json:"tool_call_hash"`
	Approved     bool   `json:"approved"`
}

func (c *Controller) sendApprovalDecision(
	ctx context.Context, workflowID, signalName, toolCallHash string, approved bool,
) (ApprovalDecision, error) {
	pending, err := c.PendingApproval(ctx, workflowID)
	if err != nil {
		return ApprovalDecision{}, fmt.Errorf("runcontroller: %s: %w", signalName, err)
	}
	if pending == nil {
		return ApprovalDecision{}, fmt.Errorf("runcontroller: %s: no approval is currently pending for %s", signalName, workflowID)
	}
	decision := map[string]any{"approval_id": pending["approval_id"], "tool_call_hash": toolCallHash}
	if err := c.Client.SignalWorkflow(ctx, workflowID, "", signalName, decision); err != nil {
		return ApprovalDecision{}, fmt.Errorf("runcontroller: %s: %w", signalName, err)
	}
	// Reported only after the signal landed. The decision becomes a FACT when the run receives it, and
	// returning it earlier would invite the caller to journal a decision that never arrived — a resuming
	// loop would then read a granted approval for a step nobody ever let through.
	return ApprovalDecision{
		ApprovalID:   stringField(pending, "approval_id"),
		NodeID:       stringField(pending, "node_id"),
		ToolCallHash: toolCallHash,
		Approved:     approved,
	}, nil
}

// stringField reads a string out of the workflow's query result, which is untyped by construction:
// it crossed a process boundary as JSON. A missing or non-string field yields "" rather than an error
// because none of these fields is worth failing a decision that already took effect.
func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// Status is RunState's status field (proto/schemas/run_state.schema.json), derived from Temporal's
// own execution status plus (while RUNNING) the workflow's own is_paused/pending_approval queries.
type Status struct {
	WorkflowID      string         `json:"workflow_id"`
	Status          string         `json:"status"`
	Paused          bool           `json:"paused"`
	PendingApproval map[string]any `json:"pending_approval,omitempty"`
	BudgetsConsumed map[string]any `json:"budgets_consumed,omitempty"`
}

// Status fetches a run's current status. budgets_consumed (RUN-003) is queried regardless of
// terminal-ness — Temporal answers queries against a closed workflow by replaying its history, so
// this still reports accurate counts after e.g. a budget-triggered failure.
func (c *Controller) Status(ctx context.Context, workflowID, tenant string) (*Status, error) {
	desc, err := c.Client.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		return nil, fmt.Errorf("runcontroller: describe: %w", err)
	}
	temporalStatus := desc.WorkflowExecutionInfo.GetStatus()

	paused := false
	var pendingApproval map[string]any
	if temporalStatus == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		if val, err := c.Client.QueryWorkflow(ctx, workflowID, "", "is_paused"); err == nil {
			_ = val.Get(&paused)
		}
		pendingApproval, _ = c.PendingApproval(ctx, workflowID)
	}

	var budgetsConsumed map[string]any
	if val, err := c.Client.QueryWorkflow(ctx, workflowID, "", "budgets_consumed"); err == nil {
		_ = val.Get(&budgetsConsumed)
	}
	c.addLedgerSpend(ctx, workflowID, tenant, &budgetsConsumed)

	return &Status{
		WorkflowID:      workflowID,
		Status:          mapStatus(temporalStatus, paused, pendingApproval != nil),
		Paused:          paused,
		PendingApproval: pendingApproval,
		BudgetsConsumed: budgetsConsumed,
	}, nil
}

// mapStatus translates Temporal's execution status into RunState's status enum
// (proto/schemas/run_state.schema.json). A pending approval takes precedence over a plain pause —
// PAUSED_FOR_APPROVAL is the more actionable of the two if somehow both were true at once.
func mapStatus(s enumspb.WorkflowExecutionStatus, paused, hasPendingApproval bool) string {
	switch s {
	case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:
		if hasPendingApproval {
			return "PAUSED_FOR_APPROVAL"
		}
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

// addLedgerSpend fills the three numbers the workflow cannot know (MDL-018).
//
// WHY THEY COME FROM HERE. `budgets_consumed` used to report `model_calls`, `tokens` and `cost_usd` as
// 0 on every run: the generic Graph Runtime has no model_call node kind and never increments them, and
// this status endpoint returned the dict verbatim. A run that had spent two dollars answered
// `cost_usd: 0.0`, which reads as "this run was free" — the DEFAULT 0 defect OBS-008 removed from the
// ledger, sitting in a user-facing API.
//
// The workflow cannot fill them: reading a database from workflow code is what docs/adr/0001 forbids,
// and the numbers would not replay. This component can — it already holds a Postgres connection for
// the circuit-breaker check — and the ledger is the same table the FinOps dashboard aggregates and the
// Model Gateway enforces the ceiling against, so the three agree by construction.
//
// WITHOUT A LEDGER THEY STAY ABSENT, which is the whole point. A deployment with no AEON_PG_DSN knows
// nothing about what a run spent, and saying so is the only honest answer — a zero there would be the
// defect this function exists to remove, reintroduced by its own fallback.
func (c *Controller) addLedgerSpend(ctx context.Context, workflowID, tenant string, consumed *map[string]any) {
	if c.Ledger == nil || tenant == "" {
		return
	}
	// The run id is the workflow id minus the prefix this controller adds when it starts a run, because
	// the ledger is keyed by the run id the CALLER chose.
	runID := strings.TrimPrefix(workflowID, WorkflowIDPrefix)
	spend, err := c.Ledger.FinOpsLedgerFor(tenant).SpendForRun(ctx, runID)
	if err != nil {
		log.Printf("runcontroller: cannot read spend for run %s, so its status omits cost: %v", runID, err)
		return
	}
	if *consumed == nil {
		*consumed = map[string]any{}
	}
	(*consumed)["model_calls"] = spend.ModelCalls
	(*consumed)["tokens"] = spend.Tokens
	(*consumed)["cost_usd"] = spend.CostUSD
	// The two bounds, reported whenever they are non-zero. A reader comparing `cost_usd` against a
	// ceiling needs to know the figure is a lower bound, and which kind — a provider can report a cost
	// and no usage, or usage and no cost.
	if spend.UnpricedCalls > 0 {
		(*consumed)["unpriced_calls"] = spend.UnpricedCalls
	}
	if spend.UnreportedUsageCalls > 0 {
		(*consumed)["unreported_usage_calls"] = spend.UnreportedUsageCalls
	}
}
