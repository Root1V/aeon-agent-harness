"""Graph Runtime (RUN-002): a deterministic recursive executor for GraphNode specs
(proto/schemas/graph_spec.schema.json — sequential/parallel/conditional/loop/subgraph/fan-in).

Per docs/adr/0001-temporal-determinism-boundary.md: this module NEVER calls a model or a tool
directly. It only calls Activities (aeon_worker.activities) and applies their typed results —
control flow (which branch, how many iterations) is decided purely from data already returned by
prior Activity calls, so it replays identically. Its only Temporal imports are
`workflow.execute_activity`/`workflow.wait_condition`/`workflow.now` and the `ApplicationError`
exception type (GraphError below) — no client, no worker, so it stays independently unit-testable.

NOTE on imports: this module does its own plain (non-`imports_passed_through`) imports, including
of aeon_worker.activities.tool_activities. It is only ever brought into a workflow's sandbox via
graph_run.py's own `imports_passed_through()` block, which already marks this whole module (and
everything it imports) as passthrough. Nesting a second `imports_passed_through()` context in here
broke Temporal's type-hint resolution for Activity results (ExecuteToolOutput) with a spurious
"name 'Any' is not defined" — the workflow task failed deterministically on every replay, which
looks like a hang (infinite backoff-retry) rather than a crash. See docs/adr/0001, "one
`imports_passed_through()` per workflow file, never nested."
"""
from __future__ import annotations

import asyncio
import hashlib
import json
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any

from temporalio import workflow
from temporalio.common import RetryPolicy
from temporalio.exceptions import ApplicationError

from aeon_worker.activities.activity_policy_activities import (
    ActivityPolicyInput,
    ActivityPolicyOutput,
    check_activity_policy_activity,
)
from aeon_worker.activities.tool_activities import ExecuteToolInput, ExecuteToolOutput, execute_tool_activity

_MISSING = object()


class GraphError(ApplicationError):
    """A structurally invalid GraphNode (unknown kind, missing required field), or a RUN-003
    budget hard stop (BudgetExceededError below).

    Subclasses temporalio's ApplicationError rather than plain Exception — this matters more than
    it looks: Temporal's Python SDK only treats ApplicationError (and subclasses) as a clean,
    terminal workflow FAILURE. Any other exception type raised from workflow code is treated as a
    workflow TASK failure and retried forever with backoff — which looks exactly like the
    sandboxing hang documented in ADR-001 (infinite retries, no obvious error) but is a completely
    unrelated cause. Discovered building RUN-003's budget hard stop: an early version of
    BudgetExceededError(Exception) hung the same way for the same underlying reason.
    """

    def __init__(self, message: str) -> None:
        super().__init__(message, type=self.__class__.__name__, non_retryable=True)


class BudgetExceededError(GraphError):
    """RUN-003's hard stop: raised the moment a limit in BudgetPolicy would be crossed, before the
    action that would cross it runs (see _execute_tool_call, and the depth/deadline checks at the
    top of execute_graph). Propagates out of the workflow as an unhandled exception, which Temporal
    surfaces as a FAILED run — this is deliberate: a budget overrun is a hard stop, not a status the
    run continues past.

    `reason` is one of tool_calls_exceeded/depth_exceeded/deadline_exceeded, and is prefixed onto
    the message (not just stored as a Python attribute): a workflow failure crossing the Temporal
    client boundary carries only its message and type string, not arbitrary extra attributes, so a
    caller inspecting the failure (a test, a future Run Controller endpoint) needs the reason to be
    parseable from the message itself.
    """

    def __init__(self, detail: str, reason: str) -> None:
        self.reason = reason
        super().__init__(f"{reason}: {detail}")


@dataclass
class BudgetPolicy:
    """Hard limits (RUN-003), all optional — None means unlimited for that dimension. Mirrors
    AgentManifest's spec.runtime.{maxDepth,deadlineSeconds,budgets.toolCalls} (examples/
    deep-research/agent.yaml). model_calls/tokens/cost_usd aren't enforced yet — not because the
    Model Gateway doesn't exist (MDL-001 is real and DONE, and OBS-003 even computes real per-call
    cost there), but because this generic Graph Runtime (graph_spec.schema.json) has no `model_call`
    node kind at all — only DR-001's own Activities call the Model Gateway today, outside this
    runtime entirely. BudgetsConsumed below declares those fields anyway so RunState's shape doesn't
    need to change when a real model_call node kind starts populating them."""

    max_tool_calls: int | None = None
    # VRT-AEON-001: external activity nodes get their OWN limit and are not folded into
    # max_tool_calls. An external activity is not a tool call — it is work handed to a worker this
    # deployment does not own — and one counter meaning two things is the defect MDL-018 removed
    # from the other end of this same run state (three zeros reported as measurements).
    max_activity_calls: int | None = None
    max_depth: int | None = None
    deadline: Any | None = None  # an absolute workflow.now()-based datetime; see workflows/graph_run.py


@dataclass
class BudgetsConsumed:
    """Mirrors run_state.schema.json's `budgets_consumed` shape."""

    tool_calls: int = 0
    activity_calls: int = 0  # VRT-AEON-001; see BudgetPolicy.max_activity_calls
    depth: int = 0  # the deepest subgraph nesting actually reached, not a cumulative count
    model_calls: int = 0
    tokens: int = 0
    cost_usd: float = 0.0


class ApprovalDeniedError(GraphError):
    """RUN-005: raised when a `requires_approval` tool_call's decision is a rejection, or an
    approval was granted for parameters that don't match the call about to run (parameter
    binding — see compute_tool_call_hash). Either way, the call never executes."""


class ApprovalExpiredError(GraphError):
    """RUN-005: raised when a `requires_approval` tool_call's TTL elapses with no decision at
    all — a silent non-decision must not be treated as an approval."""


def compute_tool_call_hash(node_id: str, tool_name: str, tool_args: dict[str, Any]) -> str:
    """RUN-005's parameter binding: an approval is only valid for the EXACT (node, tool, args) it
    was granted for. Canonical JSON (sorted keys) makes this stable regardless of dict ordering."""
    canonical = json.dumps({"node_id": node_id, "tool_name": tool_name, "tool_args": tool_args}, sort_keys=True)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def compute_activity_call_hash(node_id: str, activity_name: str, task_queue: str, args: dict[str, Any]) -> str:
    """RUN-005's parameter binding for an external activity (VRT-AEON-001).

    THE TASK QUEUE IS IN THE HASH, and that is the whole reason this is a separate function rather
    than a reuse of compute_tool_call_hash with the name slotted in. The queue decides WHICH worker
    picks the work up, so two nodes identical but for the queue are two different effects on two
    different machines. Leaving it out would make an approval granted for `acme-online` valid for
    `acme-masivo`, which is exactly the class of substitution RUN-005 exists to refuse.
    """
    canonical = json.dumps(
        {"node_id": node_id, "activity_name": activity_name, "task_queue": task_queue, "args": args},
        sort_keys=True,
    )
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def _resolve_path(obj: Any, path: str) -> Any:
    """Walks a dotted path (e.g. 'result.status') through nested dicts. Missing keys resolve to
    _MISSING (distinct from a real None) so `exists` comparisons are unambiguous."""
    current = obj
    for part in path.split("."):
        if isinstance(current, dict):
            if part not in current:
                return _MISSING
            current = current[part]
        else:
            return _MISSING
    return current


def _condition_matches(value: Any, condition: dict[str, Any]) -> bool:
    resolved = _resolve_path(value, condition["path"]) if "path" in condition else value
    if "equals" in condition:
        return resolved is not _MISSING and resolved == condition["equals"]
    if "exists" in condition:
        return (resolved is not _MISSING) == bool(condition["exists"])
    raise GraphError("condition has no supported comparator (equals/exists)")


@dataclass
class GraphExecutionState:
    """Threaded through recursive node execution.

    context: node_id -> that node's result, so a later conditional/loop node can reference an
    already-executed sibling (see graph_spec.schema.json's `condition.node_ref`).
    step_seq: a run-wide monotonic counter. Combined with each node's own id, it guarantees a
    unique idempotency key per tool_call (docs/adr/0001) even across loop iterations that reuse
    the same tool_args.
    """

    run_id: str
    # VRT-AEON-005: the tenant the RUN belongs to, read from the Temporal memo by the workflow (see
    # workflows/graph_run.py). Every governed call a step makes carries it, because the caller the
    # gateways see is this worker — one token, one tenant — and without it a shared deployment judged
    # and billed every run against the worker's tenant whoever submitted it.
    #
    # Empty is legal and means "not known": a run started before the memo existed, or a graph executed
    # outside a run. The gateways then fall back to the caller's tenant, which is the old behaviour.
    tenant: str = ""
    # TOOL-004: the principal the Tool Gateway evaluates Cedar policy against. Empty means the run
    # has no agent identity, which the gateway path refuses rather than defaulting — a call with no
    # principal is a call no policy can deny, and a default principal would make every per-agent
    # rule in the bundle apply to whoever happened to be running.
    agent_manifest_ref: str = ""
    context: dict[str, Any] = field(default_factory=dict)
    step_seq: int = 0
    # RUN-001 (Run Controller): when set, checked before every node. A caller pauses a run purely
    # by flipping whatever mutable flag this closure reads (see workflows/graph_run.py's `pause`/
    # `resume` signals) — execute_graph itself has no notion of "paused", only "wait until told to
    # proceed", which keeps this module Temporal-signal-agnostic.
    is_paused: Callable[[], bool] = lambda: False
    # RUN-003 (Budgets): limits and running counts. See BudgetPolicy/BudgetsConsumed above.
    budgets: BudgetPolicy = field(default_factory=BudgetPolicy)
    consumed: BudgetsConsumed = field(default_factory=BudgetsConsumed)
    # RUN-005 (Approvals): called for a tool_call node with requires_approval=true, given
    # (node_id, tool_call_hash, ttl_seconds). Must raise ApprovalDeniedError/ApprovalExpiredError
    # if the call must not proceed, and return normally if approved — see workflows/graph_run.py's
    # `_await_approval`. None (the default) means no approval mechanism is wired; a node that sets
    # requires_approval with this unset is a misconfiguration, not a silent pass-through (see
    # _execute_tool_call) — approval gates must never be quietly skippable.
    await_approval: Callable[[str, str, "int | None"], Awaitable[None]] | None = None
    approval_ttl_seconds: int | None = None

    def next_step_seq(self) -> int:
        self.step_seq += 1
        return self.step_seq


async def execute_graph(node: dict[str, Any], state: GraphExecutionState, depth: int = 0) -> dict[str, Any]:
    node_id = node.get("id")
    if not node_id:
        raise GraphError("every GraphNode requires an 'id'")
    kind = node.get("kind")

    # Budget checks happen before ANY node's work — including its own descent into a subgraph —
    # so a run never does one unit of work past its limit (RUN-003's "hard stop").
    if state.budgets.max_depth is not None and depth > state.budgets.max_depth:
        raise BudgetExceededError(
            f"node {node_id!r} at depth {depth} exceeds max_depth={state.budgets.max_depth}",
            reason="depth_exceeded",
        )
    state.consumed.depth = max(state.consumed.depth, depth)

    if state.budgets.deadline is not None and workflow.now() >= state.budgets.deadline:
        raise BudgetExceededError(f"deadline {state.budgets.deadline} reached at node {node_id!r}", reason="deadline_exceeded")

    if state.is_paused():
        await workflow.wait_condition(lambda: not state.is_paused())

    if kind == "tool_call":
        result = await _execute_tool_call(node, state)
    elif kind == "activity":
        result = await _execute_activity(node, state)
    elif kind == "sequential":
        result = await _execute_sequential(node, state, depth)
    elif kind == "parallel":
        result = await _execute_parallel(node, state, depth)
    elif kind == "fan_in":
        result = await _execute_fan_in(node, state, depth)
    elif kind == "conditional":
        result = await _execute_conditional(node, state, depth)
    elif kind == "loop":
        result = await _execute_loop(node, state, depth)
    elif kind == "subgraph":
        result = await _execute_subgraph(node, state, depth)
    else:
        raise GraphError(f"unknown GraphNode kind {kind!r}")

    state.context[node_id] = result
    return result


async def _execute_tool_call(node: dict[str, Any], state: GraphExecutionState) -> dict[str, Any]:
    # Checked here, not in execute_graph: this is the only place a tool_call actually runs, so
    # this guarantees the executed count never exceeds max_tool_calls even if some future node
    # kind calls a tool through a different path. Checked BEFORE incrementing: consumed.tool_calls
    # must reflect calls that actually ran, not attempts — the would-be call that trips the limit
    # never executes and must never be counted as if it had.
    if state.budgets.max_tool_calls is not None and state.consumed.tool_calls + 1 > state.budgets.max_tool_calls:
        raise BudgetExceededError(
            f"tool_calls would exceed max_tool_calls={state.budgets.max_tool_calls} at node {node['id']!r}",
            reason="tool_calls_exceeded",
        )

    if node.get("requires_approval"):
        if state.await_approval is None:
            raise GraphError(f"node {node['id']!r} sets requires_approval but no approval mechanism is wired")
        tool_call_hash = compute_tool_call_hash(node["id"], node["tool_name"], node.get("tool_args", {}))
        # Raises ApprovalDeniedError/ApprovalExpiredError and never returns if the call must not
        # proceed — the tool_calls counter below is only reached once approval is actually granted.
        await state.await_approval(node["id"], tool_call_hash, state.approval_ttl_seconds)

    state.consumed.tool_calls += 1

    inp = ExecuteToolInput(
        run_id=state.run_id,
        node_id=node["id"],
        step_seq=state.next_step_seq(),
        tool_name=node["tool_name"],
        tool_args=node.get("tool_args", {}),
        agent_manifest_ref=state.agent_manifest_ref,
        tenant=state.tenant,
    )
    output: ExecuteToolOutput = await workflow.execute_activity(
        execute_tool_activity,
        inp,
        start_to_close_timeout=timedelta(seconds=10),
        retry_policy=RetryPolicy(maximum_attempts=5),
    )
    return {
        "node_id": node["id"],
        "kind": "tool_call",
        "deduplicated": output.deduplicated,
        "idempotency_key": output.idempotency_key,
        "result": output.result,
    }


async def _execute_activity(node: dict[str, Any], state: GraphExecutionState) -> dict[str, Any]:
    """VRT-AEON-001: schedule a named Temporal activity on a task queue served by a worker OUTSIDE
    Aeon, under Aeon's governance.

    ORDER IS LOAD-BEARING: budget, then POLICY, then approval, then schedule.

    Policy before approval, and not the other way round, because asking a person to approve
    something the bundle forbids spends their attention on a decision that cannot be honoured — and
    if they say yes and we then refuse, the audit line records a human approving an action that
    never ran, which is worse than no line at all. Deny first, then ask.

    THE EFFECT IS OUTSIDE OUR PERIMETER and the comment is here so nobody oversells it later. For a
    tool, the gateway decides AND executes, so it is in the data path. Here the worker belongs to the
    consumer: Aeon's boundary is that its own workflow will not schedule unauthorized work. It does
    not stop that consumer's code from putting the same task on its own queue by itself.
    """
    node_id = node["id"]
    activity_name = node.get("activity_name")
    task_queue = node.get("task_queue")
    if not activity_name or not task_queue:
        raise GraphError(
            f"node {node_id!r} is kind=activity and needs both activity_name and task_queue: "
            "without the queue there is no statement about whose worker runs this"
        )

    # Checked BEFORE incrementing, like the tool counter: consumed.activity_calls must count work
    # that actually ran, so the call that trips the limit is never counted as if it had.
    if state.budgets.max_activity_calls is not None and state.consumed.activity_calls + 1 > state.budgets.max_activity_calls:
        raise BudgetExceededError(
            f"activity_calls would exceed max_activity_calls={state.budgets.max_activity_calls} at node {node_id!r}",
            reason="activity_calls_exceeded",
        )

    args = node.get("args", {})
    requires_approval = bool(node.get("requires_approval"))

    decision: ActivityPolicyOutput = await workflow.execute_activity(
        check_activity_policy_activity,
        ActivityPolicyInput(
            agent_manifest_ref=state.agent_manifest_ref,
            activity_name=activity_name,
            task_queue=task_queue,
            requires_approval=requires_approval,
            tenant=state.tenant,
        ),
        start_to_close_timeout=timedelta(seconds=30),
        retry_policy=RetryPolicy(maximum_attempts=5),
    )

    if requires_approval:
        if state.await_approval is None:
            raise GraphError(f"node {node_id!r} sets requires_approval but no approval mechanism is wired")
        approval_hash = compute_activity_call_hash(node_id, activity_name, task_queue, args)
        # Raises and never returns if the work must not proceed, so the counter below is only
        # reached once a person has actually said yes.
        await state.await_approval(node_id, approval_hash, state.approval_ttl_seconds)

    state.consumed.activity_calls += 1

    # TIMEOUTS AND RETRIES COME FROM THE NODE, which is the half of this feature that the hardcoded
    # 10s/5-attempts of _execute_tool_call cannot express. A tool call is a short request to a
    # gateway we run; an external activity is OCR or inference on somebody else's machine, measured
    # in minutes.
    timeout_seconds = int(node.get("timeout_seconds", 600))
    retry = node.get("retry", {}) or {}
    retry_policy = RetryPolicy(
        maximum_attempts=int(retry.get("maximum_attempts", 3)),
        initial_interval=timedelta(seconds=float(retry.get("initial_interval_seconds", 1))),
        backoff_coefficient=float(retry.get("backoff", 2.0)),
        non_retryable_error_types=list(retry.get("non_retryable_errors", []) or []),
    )

    # HEARTBEAT IS A CONTRACT WITH THE OTHER TEAM'S WORKER, not something Aeon can provide. Setting
    # heartbeat_timeout only makes Temporal EXPECT a heartbeat; emitting one is the activity
    # implementation's job. A node that declares 30s against a worker that never calls heartbeat()
    # fails at 30s, not at timeout_seconds — so this is omitted unless the graph asks for it, rather
    # than defaulted to a value that would turn a working long activity into a timing out one.
    heartbeat_seconds = node.get("heartbeat_seconds")
    heartbeat_timeout = timedelta(seconds=int(heartbeat_seconds)) if heartbeat_seconds else None

    result = await workflow.execute_activity(
        activity_name,
        args,
        task_queue=task_queue,
        start_to_close_timeout=timedelta(seconds=timeout_seconds),
        heartbeat_timeout=heartbeat_timeout,
        retry_policy=retry_policy,
    )

    return {
        "node_id": node_id,
        "kind": "activity",
        "activity_name": activity_name,
        "task_queue": task_queue,
        # The policy that permitted this, carried into the result so the run's own record says WHY
        # it was allowed to hand work outside — the same reason a denial carries its policy id.
        "policy_id": decision.policy_id,
        "result": result,
    }


async def _execute_sequential(node: dict[str, Any], state: GraphExecutionState, depth: int) -> dict[str, Any]:
    results = []
    for child in node.get("children", []):
        results.append(await execute_graph(child, state, depth))
    return {"node_id": node["id"], "kind": "sequential", "children": results}


async def _execute_parallel(node: dict[str, Any], state: GraphExecutionState, depth: int) -> dict[str, Any]:
    # asyncio.gather over workflow.execute_activity calls is Temporal's documented pattern for
    # concurrent Activities inside a deterministic workflow: command order is recorded in history,
    # so replay reproduces the same completion order even though real-time execution is concurrent.
    results = await asyncio.gather(*(execute_graph(child, state, depth) for child in node.get("children", [])))
    return {"node_id": node["id"], "kind": "parallel", "children": list(results)}


async def _execute_fan_in(node: dict[str, Any], state: GraphExecutionState, depth: int) -> dict[str, Any]:
    results = await asyncio.gather(*(execute_graph(child, state, depth) for child in node.get("children", [])))
    merged = [r.get("result", r) for r in results]
    return {"node_id": node["id"], "kind": "fan_in", "merged": merged, "children": list(results)}


async def _execute_conditional(node: dict[str, Any], state: GraphExecutionState, depth: int) -> dict[str, Any]:
    condition = node["condition"]
    referenced = state.context.get(condition["node_ref"], _MISSING)
    taken_true = referenced is not _MISSING and _condition_matches(referenced, condition)
    branch = node["if_true"] if taken_true else node.get("if_false")

    if branch is None:
        return {"node_id": node["id"], "kind": "conditional", "taken": "none", "branch": None}

    result = await execute_graph(branch, state, depth)
    return {
        "node_id": node["id"],
        "kind": "conditional",
        "taken": "if_true" if taken_true else "if_false",
        "branch": result,
    }


async def _execute_loop(node: dict[str, Any], state: GraphExecutionState, depth: int) -> dict[str, Any]:
    max_iterations = node["max_iterations"]
    if max_iterations < 1:
        raise GraphError(f"loop node {node['id']!r}: max_iterations must be >= 1")
    stop_condition = node.get("stop_condition")

    iterations = []
    stopped_reason = "max_iterations"
    for i in range(max_iterations):
        body = dict(node["body"])
        body["id"] = f"{node['body']['id']}[{i}]"
        iteration_result = await execute_graph(body, state, depth)
        iterations.append(iteration_result)
        if stop_condition and _condition_matches(iteration_result, stop_condition):
            stopped_reason = "condition_met"
            break

    return {"node_id": node["id"], "kind": "loop", "iterations": iterations, "stopped_reason": stopped_reason}


async def _execute_subgraph(node: dict[str, Any], state: GraphExecutionState, depth: int) -> dict[str, Any]:
    # Wraps the inner graph's result rather than returning it verbatim: every other kind wraps its
    # children's results under its own node_id, and a "transparent" subgraph would make the outer
    # node's own id unfindable in the result tree (state.context[node_id] would record a dict whose
    # own "node_id" field is the INNER graph's, not this subgraph node's).
    inner_result = await execute_graph(node["graph"], state, depth + 1)
    return {"node_id": node["id"], "kind": "subgraph", "graph_result": inner_result}
