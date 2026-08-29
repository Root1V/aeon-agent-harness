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
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any

from temporalio import workflow
from temporalio.common import RetryPolicy
from temporalio.exceptions import ApplicationError

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
    deep-research/agent.yaml). model_calls/tokens/cost_usd aren't enforced yet: the Graph Runtime
    has no Model Gateway call site to count them from (MDL-001 is still TODO) — BudgetsConsumed
    below declares those fields anyway so RunState's shape doesn't need to change when they start
    being real."""

    max_tool_calls: int | None = None
    max_depth: int | None = None
    deadline: Any | None = None  # an absolute workflow.now()-based datetime; see workflows/graph_run.py


@dataclass
class BudgetsConsumed:
    """Mirrors run_state.schema.json's `budgets_consumed` shape."""

    tool_calls: int = 0
    depth: int = 0  # the deepest subgraph nesting actually reached, not a cumulative count
    model_calls: int = 0
    tokens: int = 0
    cost_usd: float = 0.0


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
    state.consumed.tool_calls += 1

    inp = ExecuteToolInput(
        run_id=state.run_id,
        node_id=node["id"],
        step_seq=state.next_step_seq(),
        tool_name=node["tool_name"],
        tool_args=node.get("tool_args", {}),
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
