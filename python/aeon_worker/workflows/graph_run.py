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

OBS-010: that wait is also REPORTED, via record_approval_wait_activity — once when it begins
(`argus.outcome=suspended`) and once when it ends (`ok`/`denied`/`timeout`). See
aeon_worker/activities/approval_activities.py for why it is an Activity, why there are two records
and why a failure to write one never fails the run.
"""
from __future__ import annotations

from datetime import datetime, timedelta
from typing import Any

from temporalio import workflow
from temporalio.common import RetryPolicy

with workflow.unsafe.imports_passed_through():
    # aeon_worker.activities.tool_activities is imported here too (not just inside
    # aeon_worker.graph) even though this file never uses ExecuteToolInput/Output/
    # execute_tool_activity directly. Without this, Temporal's sandbox reload of THIS file (the
    # one carrying @workflow.defn) does not register tool_activities as passthrough for the
    # nested import inside aeon_worker.graph, and Activity result decoding fails deterministically
    # with "name 'Any' is not defined" the first time execute_tool_activity's dataclass return type
    # is resolved (see graph.py's module docstring and docs/adr/0001).
    from aeon_worker.activities.approval_activities import (
        OUTCOME_DENIED,
        OUTCOME_EXPIRED,
        OUTCOME_GRANTED,
        OUTCOME_SUSPENDED,
        ApprovalWaitInput,
        record_approval_wait_activity,
    )
    from aeon_worker.activities.tool_activities import ExecuteToolInput, ExecuteToolOutput, execute_tool_activity
    from aeon_worker.graph import (
        ApprovalDeniedError,
        ApprovalExpiredError,
        BudgetPolicy,
        GraphExecutionState,
        execute_graph,
    )

_ = (ExecuteToolInput, ExecuteToolOutput, execute_tool_activity)  # imported for their passthrough side effect only


# OBS-010's patch id. Temporal writes a marker under this exact string into the history of every run
# that takes the new path, so it is a name that can never be edited without stranding the runs that
# already recorded it — it is data in a durable store, not a label.
PATCH_APPROVAL_WAIT_RECORDED = "obs-010-approval-wait-recorded"


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
        # OBS-010's patch decision, read once per wait and reused by every record in it.
        self._record_waits = False
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
        """What this workflow has actually counted — and nothing it has not (MDL-018).

        THIS USED TO REPORT THREE ZEROS AS MEASUREMENTS. `model_calls`, `tokens` and `cost_usd` were
        returned as 0 on every run, because this generic Graph Runtime has no `model_call` node kind and
        never increments them — and `GET /runs/{id}` surfaces this dict verbatim. So a run that had
        spent two dollars answered `cost_usd: 0.0`, which reads as "this run was free". It is the
        `DEFAULT 0` defect OBS-008 removed from the cost ledger, sitting in a user-facing API.

        The three are omitted now, and the Run Controller fills them from the FinOps ledger, which is
        the component that actually knows (go/internal/runcontroller). The workflow cannot: reading a
        database from workflow code is exactly what docs/adr/0001 forbids, and the numbers would not
        replay.

        Absent and not null, because the two keys left are the ones this workflow measures itself, and a
        consumer that sees `cost_usd` missing has to go and ask rather than believe a zero.
        """
        if self._state is None:
            return {"tool_calls": 0, "depth": 0}
        c = self._state.consumed
        return {"tool_calls": c.tool_calls, "depth": c.depth}

    async def _record_approval_wait(
        self, node_id: str, approval_id: str, tool_call_hash: str, outcome: str, waited_ms: int | None = None
    ) -> None:
        """OBS-010: write one `approval.wait` record. Never fails the run, and never breaks an old one.

        THE PATCH IS NOT PRECAUTIONARY, IT WAS MEASURED. Replaying a history recorded before this change
        against this code fails outright:

            [TMPRL1100] Nondeterminism error: Activity type of scheduled event
            'execute_tool_activity' does not match activity type of activity command
            'record_approval_wait'

        Scheduling an Activity from workflow code adds a command to the history, and a run that recorded
        its history without that command cannot be resumed by code that now produces it. The runs this
        would have killed are exactly the ones this feature exists for: a run parked on a human decision
        is the longest-lived thing in the system, and it dies on its next workflow task — the approval
        signal that finally arrives. An observability feature taking out the waits it was built to
        observe, with the damage falling on whoever waited longest.

        `workflow.patched` is the answer Temporal ships for this: a run whose history has the marker
        takes the new path, one without it takes the old one, and the check writes no command when it
        replays a history that predates it. The flag is read once and reused by every record in the wait,
        because a wait that opened without records must not close with them.

        TELEMETRY THAT CAN FAIL A RUN IS WORSE THAN NO TELEMETRY, and here it would fail it in the most
        expensive place: after a human has already decided. The Activity is given one attempt and a short
        timeout, and anything it raises is logged and dropped — a run blocked because the collector was
        down would turn an observability gap into an outage, which is the same trade `init_tracing`
        refuses on the Python side and `Init` refuses on the Go side.
        """
        if not self._record_waits:
            return
        try:
            await workflow.execute_activity(
                record_approval_wait_activity,
                ApprovalWaitInput(
                    run_id=self._state.run_id if self._state else "",
                    approval_id=approval_id,
                    node_id=node_id,
                    tool_call_hash=tool_call_hash,
                    outcome=outcome,
                    waited_ms=waited_ms,
                ),
                start_to_close_timeout=timedelta(seconds=10),
                retry_policy=RetryPolicy(maximum_attempts=1),
            )
        except Exception as exc:  # noqa: BLE001 - see the docstring
            workflow.logger.warning("approval wait record (%s) not written: %s", outcome, exc)

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

        # workflow.now() and not a wall clock: this is workflow code, and the duration derived from it is
        # replayed identically (docs/adr/0001). A time.time() here would make every replay compute a
        # different wait and turn a report into a source of non-determinism.
        started_at = workflow.now()

        # OBS-010's patch gate. Read HERE, before the first record and before the wait, because this is
        # the point where the old code and the new one diverge — a run whose history has no marker keeps
        # the old path for the rest of its life, records included. See _record_approval_wait.
        self._record_waits = workflow.patched(PATCH_APPROVAL_WAIT_RECORDED)

        # OBS-010: recorded BEFORE blocking, which is the half that has to be first. A record written only
        # when the wait ends says nothing during the hours it is the only thing happening.
        await self._record_approval_wait(node_id, approval_id, tool_call_hash, OUTCOME_SUSPENDED)

        try:
            await workflow.wait_condition(
                lambda: self._approval_decision is not None,
                timeout=timedelta(seconds=ttl_seconds) if ttl_seconds is not None else None,
            )
        except TimeoutError as exc:
            self._pending_approval = None
            await self._record_approval_wait(
                node_id, approval_id, tool_call_hash, OUTCOME_EXPIRED, self._waited_ms(started_at)
            )
            raise ApprovalExpiredError(f"approval {approval_id} for node {node_id!r} expired after {ttl_seconds}s") from exc

        decision = self._approval_decision
        waited_ms = self._waited_ms(started_at)
        self._pending_approval = None
        self._approval_decision = None
        assert decision is not None  # wait_condition only returns once this is set

        # The closing record goes out on EVERY path, including the two that raise. A wait whose opening
        # record has no closing one reads as "still waiting" forever, so a denial that skipped this would
        # leave a permanently suspended approval in the store for a run that ended minutes later.
        if decision["tool_call_hash"] != tool_call_hash:
            await self._record_approval_wait(node_id, approval_id, tool_call_hash, OUTCOME_DENIED, waited_ms)
            raise ApprovalDeniedError(
                f"approval decision for node {node_id!r} named a different tool_call_hash than what's "
                "executing now — a decision only applies to the exact parameters it was made for"
            )
        if not decision["approved"]:
            await self._record_approval_wait(node_id, approval_id, tool_call_hash, OUTCOME_DENIED, waited_ms)
            raise ApprovalDeniedError(f"approval for node {node_id!r} was rejected (approval_id={approval_id})")
        await self._record_approval_wait(node_id, approval_id, tool_call_hash, OUTCOME_GRANTED, waited_ms)

    @staticmethod
    def _waited_ms(started_at: datetime) -> int:
        """How long the wait lasted, measured in workflow time at both ends."""
        return int((workflow.now() - started_at).total_seconds() * 1000)

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
