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
	failurepb "go.temporal.io/api/failure/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/aeon-ai/aeon/go/internal/store"
)

// TenantMemoKey is the workflow memo field carrying the run's tenant (VRT-AEON-005 T-6).
const TenantMemoKey = "aeon_tenant"

// DefaultTenant is what a run with NO tenant memo is treated as belonging to.
//
// Chosen rather than invented: it is exactly what migration 0002 did with every pre-existing row, so
// the statement is the same one — the data and the runs that predate tenancy belong to `default`,
// which on a single-tenant deployment is true. The alternatives were worse in both directions:
// treating an un-memoed run as visible to everyone is the fallback defect this whole feature
// removes, and treating it as visible to nobody would break every run already in flight.
const DefaultTenant = "default"

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
func (c *Controller) Start(ctx context.Context, runID string, graph map[string]any, budgets map[string]any, agentManifestRef, tenant string) (*RunInfo, error) {
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
	// VRT-AEON-005 T-6: the tenant goes in the workflow's MEMO, so every later operation on this run
	// can ask Temporal who it belongs to without a table of our own.
	//
	// A MEMO AND NOT A SEARCH ATTRIBUTE, which is the "o equivalente" in Veritium's requirement. A
	// custom search attribute has to be registered in the Temporal namespace — deployment
	// configuration, and a run started before it existed would be unattributable — and what it buys
	// is FILTERING a list. There is no list-runs endpoint here, so it would be configuration bought
	// for a surface that does not exist. When one exists, that is when the search attribute earns
	// its keep; the memo is what answers "whose run is this" today, which is what T-6 asks.
	run, err := c.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: c.TaskQueue,
		Memo:      map[string]any{TenantMemoKey: tenant},
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

// Status is the response of GET /runs/{run_id}. It is NOT a RunState document, and saying so is a
// correction: this comment claimed it was, and run_state.schema.json describes durable state whose
// `graph_cursor` and `no_progress_counter` appear NOWHERE in this repository outside that file
// (measured 2026-10-08). The fields the two genuinely share are `status`, `pending_approval`,
// `budgets_consumed` and now `failure`; `run_id`/`agent_manifest_ref`/`created_at` are required
// there and absent here, which is why nothing could ever have validated this against it.
//
// Status itself is derived from Temporal's
// own execution status plus (while RUNNING) the workflow's own is_paused/pending_approval queries.
type Status struct {
	WorkflowID      string         `json:"workflow_id"`
	Status          string         `json:"status"`
	Paused          bool           `json:"paused"`
	PendingApproval map[string]any `json:"pending_approval,omitempty"`
	BudgetsConsumed map[string]any `json:"budgets_consumed,omitempty"`
	// Failure is why a run ended badly, and it is ABSENT unless something went wrong in a way
	// `Status` cannot express (OBS-011 — numbered 011 and not 010 because roadmap_check's duplicate-id
	// guard caught me reusing an id that already names another feature; this index is cited by id
	// across teams, so one id names one thing).
	//
	// WHY IT EXISTS: Veritium asked for it as a second-consumer note on VRT-AEON-001 — "GET
	// /runs/{id} no dice por qué falló un run. Como aeon no avisa, marcamos nuestra corrida fallida
	// con un reconciliador que consulta el estado" — and said they were not asking for anything. A
	// consumer running a reconciliator that can see THAT a run failed and not WHY has to go to
	// Temporal's own UI to find out, which is the one place a platform consumer should not have to
	// look.
	//
	// Not set for CANCELLED: `Status` already says that, and a Temporal cancellation carries no
	// reason, so a `failure` object there would be a field that exists to say nothing.
	Failure *Failure `json:"failure,omitempty"`
}

// Failure is the reason a run ended badly, read from the workflow's CLOSE EVENT.
//
// THE THREE KINDS ARE THE POINT. `mapStatus` maps Temporal's FAILED, TIMED_OUT and TERMINATED onto
// one `FAILED`, because run_state.schema.json's status enum has one value for all three and widening
// it would break every consumer that switches on it. That merge is fine for a category and wrong as
// the whole answer: a workflow that raised an error, one that ran out of time and one an operator
// killed are three different incidents with three different next actions. `Kind` is where the
// distinction lives.
type Failure struct {
	// Kind is "failed", "timed_out" or "terminated".
	Kind string `json:"kind"`
	// Message is the ROOT CAUSE's message and not the outermost one, measured: an activity failure's
	// outer message is the literal string "activity error" and the useful text is one level down.
	Message string `json:"message,omitempty"`
	// Type is the application error type when the failure carries one (`BudgetExceeded`,
	// `PolicyDenied`). It is the workflow's own vocabulary, which is what makes it worth recording:
	// a consumer can branch on it without parsing a message.
	Type string `json:"type,omitempty"`
	// Activity names which activity failed, when the failure came from one. It is at the OUTER level
	// of the chain while the message is at the inner, so both have to be collected separately.
	Activity string `json:"activity,omitempty"`
	// Retryable is a POINTER because it has three states: Temporal said non-retryable, Temporal said
	// retryable, or the failure carried no application info to say either. A bool would make the
	// third indistinguishable from the second — the same fabricated-default shape MDL-014 removed
	// from the token counters.
	Retryable *bool `json:"retryable,omitempty"`
	// TerminatedBy is Temporal's `identity` on a termination: a worker or CLI identity string, NOT an
	// authenticated Aeon caller. Named for what it is, because calling it an actor would overclaim —
	// nothing here proves who the person was.
	TerminatedBy string `json:"terminated_by,omitempty"`
}

// MESSAGES COME FROM THE WORKFLOW AND ARE PASSED THROUGH UNCHANGED, which is a decision and not an
// oversight. They are the activity's or the workflow's own text, so they can contain whatever that
// code put in them. Two things make that acceptable here and both are load-bearing: the route is
// gated by `ownedRun`, so only the run's OWN tenant can read it (GOV-001e), and the alternative —
// a sanitised or truncated message — would reproduce the defect this feature exists to fix, a
// consumer that can see a failure and not act on it.
//
// What must never arrive here is a chain of thought. run_state.schema.json says the durable state
// "must never contain a chain-of-thought field", and a failure message is the obvious back door: an
// activity that puts a model's reasoning into an error message publishes it. That is a rule for
// whoever writes an activity, and it is written down here because this is the surface that would
// carry it out of the process.
func (c *Controller) failureOf(ctx context.Context, workflowID string) *Failure {
	iter := c.Client.GetWorkflowHistory(ctx, workflowID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_CLOSE_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			// Best-effort, like the budget query above: a status read must not fail because the reason
			// could not be fetched. The caller still gets the status, which is what it had before.
			log.Printf("aeon-runcontroller: reading the close event for run %s: %v", workflowID, err)
			return nil
		}
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED:
			return failureFromProto(event.GetWorkflowExecutionFailedEventAttributes().GetFailure())
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TIMED_OUT:
			return &Failure{
				Kind: "timed_out",
				// Temporal reports no message for a timeout, so this one is ours — and it says which
				// timeout, because "the run exceeded its execution timeout" and "a step did" are
				// different problems and the close event only ever means the first.
				Message: "the run exceeded its workflow execution timeout",
			}
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TERMINATED:
			attrs := event.GetWorkflowExecutionTerminatedEventAttributes()
			return &Failure{
				Kind: "terminated",
				// The operator's reason, which is the ONE piece of this that `WorkflowRun.Get` does not
				// return — measured: it answers the bare string "terminated". That is why this reads the
				// history rather than the run's error.
				Message:      attrs.GetReason(),
				TerminatedBy: attrs.GetIdentity(),
			}
		}
		// Any other close event (completed, cancelled, continued-as-new) is not a failure, and the
		// caller already decided not to ask about those.
		return nil
	}
	return nil
}

// failureFromProto walks the cause chain once, collecting what each level is the only place to find.
func failureFromProto(f *failurepb.Failure) *Failure {
	if f == nil {
		return nil
	}
	out := &Failure{Kind: "failed"}
	// Bounded, because a cause chain is attacker-influenced in the same sense a payload is: it comes
	// from whatever the activity constructed. Ten is far past anything real.
	for depth := 0; f != nil && depth < 10; depth++ {
		if name := f.GetActivityFailureInfo().GetActivityType().GetName(); name != "" && out.Activity == "" {
			out.Activity = name
		}
		if app := f.GetApplicationFailureInfo(); app != nil {
			if app.GetType() != "" {
				out.Type = app.GetType()
			}
			nonRetryable := app.GetNonRetryable()
			retryable := !nonRetryable
			out.Retryable = &retryable
		}
		// The message of the DEEPEST level wins: the outer one is a wrapper ("activity error") and the
		// inner one is what the code actually said.
		if msg := f.GetMessage(); msg != "" {
			out.Message = msg
		}
		f = f.GetCause()
	}
	return out
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

	// ONLY for the three closes that have a reason, so the common paths cost nothing extra: a RUNNING
	// run and a SUCCEEDED one never read the history. That matters because a consumer's reconciliator
	// polls this endpoint — Veritium's does — and the happy path is almost all of the traffic.
	var failure *Failure
	switch temporalStatus {
	case enumspb.WORKFLOW_EXECUTION_STATUS_FAILED,
		enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT,
		enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		failure = c.failureOf(ctx, workflowID)
	}

	return &Status{
		WorkflowID:      workflowID,
		Status:          mapStatus(temporalStatus, paused, pendingApproval != nil),
		Paused:          paused,
		PendingApproval: pendingApproval,
		BudgetsConsumed: budgetsConsumed,
		Failure:         failure,
	}, nil
}

// mapStatus translates Temporal's execution status into the status enum
// (proto/schemas/run_state.schema.json's `status`). A pending approval takes precedence over a plain
// pause — PAUSED_FOR_APPROVAL is the more actionable of the two if somehow both were true at once.
//
// IT MERGES FAILED, TIMED_OUT AND TERMINATED INTO ONE `FAILED`, and that merge is kept rather than
// fixed: the enum has one value for all three and widening it would break every consumer that
// switches on it. What changed in OBS-011 is that the distinction is no longer LOST — `Status.Failure.Kind`
// carries it, so the category stays stable and the three facts stay separable.
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

// TenantOf reports which tenant a run belongs to, from its workflow memo.
//
// A run with no memo predates T-6 and is treated as DefaultTenant — see that constant for why that
// is the same statement migration 0002 made about pre-existing rows, rather than a guess.
func (c *Controller) TenantOf(ctx context.Context, workflowID string) (string, error) {
	desc, err := c.Client.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		return "", fmt.Errorf("runcontroller: describe: %w", err)
	}
	memo := desc.WorkflowExecutionInfo.GetMemo()
	if memo == nil {
		return DefaultTenant, nil
	}
	payload, ok := memo.GetFields()[TenantMemoKey]
	if !ok {
		return DefaultTenant, nil
	}
	var tenant string
	if err := converter.GetDefaultDataConverter().FromPayload(payload, &tenant); err != nil {
		return "", fmt.Errorf("runcontroller: reading the tenant memo: %w", err)
	}
	if tenant == "" {
		return DefaultTenant, nil
	}
	return tenant, nil
}
