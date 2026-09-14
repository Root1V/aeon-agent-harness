"""GraphRunWorkflow: RUN-002's Graph Runtime entrypoint, and RUN-001's Run Controller target.
Executes a GraphNode (proto/schemas/graph_spec.schema.json) to completion via
aeon_worker.graph.execute_graph.

Deterministic per docs/adr/0001-temporal-determinism-boundary.md: every non-deterministic
operation happens inside execute_graph's Activity calls, never here. This supersedes
AgentRunWorkflow (workflows/agent_run.py) as the general-purpose entrypoint; AgentRunWorkflow stays
as-is because tests/integration/test_crash_resume.py pins its exact single-node shape.

RUN-001 control surface: `pause`/`resume` signals and an `is_paused` query, driven by
go/internal/runcontroller (aeon-runcontroller). start/cancel/status/stream need no workflow-side
code at all — they're native Temporal client operations (StartWorkflow, CancelWorkflow,
DescribeWorkflowExecution) the Run Controller calls directly.

RUN-003 control surface: an optional `request["budgets"]` dict — `max_tool_calls`, `max_depth`,
`deadline_seconds` — becomes a graph.BudgetPolicy enforced by execute_graph itself (hard stop: a
limit crossed raises BudgetExceededError, which Temporal surfaces as a FAILED run). The
`budgets_consumed` query exposes the running counts, including after a budget-triggered failure —
Temporal can still answer queries against a closed workflow by replaying its history.

RUN-005 control surface: a GraphNode with `requires_approval: true` (only meaningful on
`kind: tool_call`) blocks on `_await_approval` below until an `approve`/`reject` signal arrives or
its TTL (`request["approvals"]["default_ttl_seconds"]`) elapses. `approve`/`reject` both require
the caller to name the exact `tool_call_hash` they're deciding on (parameter binding,
graph.compute_tool_call_hash) — a decision naming a different hash than the one currently pending
is rejected, not silently accepted. `pending_approval` exposes the shape RunState's
`pending_approval` field expects (proto/schemas/run_state.schema.json).
"""
from __future__ import annotations

from datetime import timedelta
from typing import Any

from temporalio import workflow

with workflow.unsafe.imports_passed_through():
    # aeon_worker.activities.tool_activities is imported here too (not just inside
    # aeon_worker.graph) even though this file never uses ExecuteToolInput/Output/
    # execute_tool_activity directly. Without this, Temporal's sandbox reload of THIS file (the
    # one carrying @workflow.defn) does not register tool_activities as passthrough for the
    # nested import inside aeon_worker.graph, and Activity result decoding fails deterministically
    # with "name 'Any' is not defined" the first time execute_tool_activity's dataclass return type
    # is resolved (see graph.py's module docstring and docs/adr/0001).
    from aeon_worker.activities.tool_activities import ExecuteToolInput, ExecuteToolOutput, execute_tool_activity
    from aeon_worker.graph import (
        ApprovalDeniedError,
        ApprovalExpiredError,
        BudgetPolicy,
        GraphExecutionState,
        execute_graph,
    )

_ = (ExecuteToolInput, ExecuteToolOutput, execute_tool_activity)  # imported for their passthrough side effect only


def _budget_policy_from_request(budgets_req: dict[str, Any]) -> BudgetPolicy:
    deadline = None
    if budgets_req.get("deadline_seconds") is not None:
        # workflow.now() is the deterministic, replay-safe wall clock — never datetime.now()
        # (docs/adr/0001). Computed once, here, into an absolute point in time.
        deadline = workflow.now() + timedelta(seconds=budgets_req["deadline_seconds"])
    return BudgetPolicy(
        max_tool_calls=budgets_req.get("max_tool_calls"),
        max_depth=budgets_req.get("max_depth"),
        deadline=deadline,
    )


@workflow.defn
class GraphRunWorkflow:
    def __init__(self) -> None:
        self._paused = False
        self._state: GraphExecutionState | None = None
        self._pending_approval: dict[str, Any] | None = None
        self._approval_decision: dict[str, Any] | None = None

    @workflow.signal
    async def pause(self) -> None:
        self._paused = True

    @workflow.signal
    async def resume(self) -> None:
        self._paused = False

    @workflow.signal
    async def approve(self, decision: dict[str, Any]) -> None:
        # A single structured payload, not two positional args: Temporal's Go client can only
        # encode ONE argument per SignalWorkflow call (dc.ToPayloads(arg) wraps exactly one value),
        # so a signal any non-Python SDK needs to call must take a single dict/dataclass, never
        # multiple positional params — go/internal/runcontroller relies on this.
        self._approval_decision = {"approval_id": decision["approval_id"], "tool_call_hash": decision["tool_call_hash"], "approved": True}

    @workflow.signal
    async def reject(self, decision: dict[str, Any]) -> None:
        self._approval_decision = {"approval_id": decision["approval_id"], "tool_call_hash": decision["tool_call_hash"], "approved": False}

    @workflow.query
    def is_paused(self) -> bool:
        return self._paused

    @workflow.query
    def pending_approval(self) -> dict[str, Any] | None:
        return self._pending_approval

    @workflow.query
    def budgets_consumed(self) -> dict[str, Any]:
        if self._state is None:
            return {"tool_calls": 0, "depth": 0, "model_calls": 0, "tokens": 0, "cost_usd": 0.0}
        c = self._state.consumed
        return {"tool_calls": c.tool_calls, "depth": c.depth, "model_calls": c.model_calls, "tokens": c.tokens, "cost_usd": c.cost_usd}

    async def _await_approval(self, node_id: str, tool_call_hash: str, ttl_seconds: int | None) -> None:
        approval_id = str(workflow.uuid4())
        expires_at = workflow.now() + timedelta(seconds=ttl_seconds) if ttl_seconds is not None else None
        self._pending_approval = {
            "approval_id": approval_id,
            "node_id": node_id,
            "tool_call_hash": tool_call_hash,
            "expires_at": expires_at.isoformat() if expires_at else None,
        }
        self._approval_decision = None

        try:
            await workflow.wait_condition(
                lambda: self._approval_decision is not None,
                timeout=timedelta(seconds=ttl_seconds) if ttl_seconds is not None else None,
            )
        except TimeoutError as exc:
            self._pending_approval = None
            raise ApprovalExpiredError(f"approval {approval_id} for node {node_id!r} expired after {ttl_seconds}s") from exc

        decision = self._approval_decision
        self._pending_approval = None
        self._approval_decision = None
        assert decision is not None  # wait_condition only returns once this is set

        if decision["tool_call_hash"] != tool_call_hash:
            raise ApprovalDeniedError(
                f"approval decision for node {node_id!r} named a different tool_call_hash than what's "
                "executing now — a decision only applies to the exact parameters it was made for"
            )
        if not decision["approved"]:
            raise ApprovalDeniedError(f"approval for node {node_id!r} was rejected (approval_id={approval_id})")

    @workflow.run
    async def run(self, request: dict[str, Any]) -> dict[str, Any]:
        budgets = _budget_policy_from_request(request.get("budgets") or {})
        approvals_req = request.get("approvals") or {}
        state = GraphExecutionState(
            run_id=request["run_id"],
            agent_manifest_ref=request.get("agent_manifest_ref", ""),
            is_paused=lambda: self._paused,
            budgets=budgets,
            await_approval=self._await_approval,
            approval_ttl_seconds=approvals_req.get("default_ttl_seconds"),
        )
        self._state = state
        result = await execute_graph(request["graph"], state)
        return {"run_id": request["run_id"], "result": result}
