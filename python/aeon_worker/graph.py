"""Graph Runtime (RUN-002): a deterministic recursive executor for GraphNode specs
(proto/schemas/graph_spec.schema.json — sequential/parallel/conditional/loop/subgraph/fan-in).

Per docs/adr/0001-temporal-determinism-boundary.md: this module NEVER calls a model or a tool
directly. It only calls Activities (aeon_worker.activities) and applies their typed results —
control flow (which branch, how many iterations) is decided purely from data already returned by
prior Activity calls, so it replays identically. It is meant to run inside a Temporal workflow
(see workflows/graph_run.py), but has no Temporal import beyond `workflow.execute_activity`, which
keeps it independently unit-testable if needed later.

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
from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any

from temporalio import workflow
from temporalio.common import RetryPolicy

from aeon_worker.activities.tool_activities import ExecuteToolInput, ExecuteToolOutput, execute_tool_activity

_MISSING = object()


class GraphError(Exception):
    """A structurally invalid GraphNode (unknown kind, missing required field). Fails the run
    fast rather than silently no-op'ing a malformed graph."""


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

    def next_step_seq(self) -> int:
        self.step_seq += 1
        return self.step_seq


async def execute_graph(node: dict[str, Any], state: GraphExecutionState) -> dict[str, Any]:
    node_id = node.get("id")
    if not node_id:
        raise GraphError("every GraphNode requires an 'id'")
    kind = node.get("kind")

    if kind == "tool_call":
        result = await _execute_tool_call(node, state)
    elif kind == "sequential":
        result = await _execute_sequential(node, state)
    elif kind == "parallel":
        result = await _execute_parallel(node, state)
    elif kind == "fan_in":
        result = await _execute_fan_in(node, state)
    elif kind == "conditional":
        result = await _execute_conditional(node, state)
    elif kind == "loop":
        result = await _execute_loop(node, state)
    elif kind == "subgraph":
        result = await _execute_subgraph(node, state)
    else:
        raise GraphError(f"unknown GraphNode kind {kind!r}")

    state.context[node_id] = result
    return result


async def _execute_tool_call(node: dict[str, Any], state: GraphExecutionState) -> dict[str, Any]:
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


async def _execute_sequential(node: dict[str, Any], state: GraphExecutionState) -> dict[str, Any]:
    results = []
    for child in node.get("children", []):
        results.append(await execute_graph(child, state))
    return {"node_id": node["id"], "kind": "sequential", "children": results}


async def _execute_parallel(node: dict[str, Any], state: GraphExecutionState) -> dict[str, Any]:
    # asyncio.gather over workflow.execute_activity calls is Temporal's documented pattern for
    # concurrent Activities inside a deterministic workflow: command order is recorded in history,
    # so replay reproduces the same completion order even though real-time execution is concurrent.
    results = await asyncio.gather(*(execute_graph(child, state) for child in node.get("children", [])))
    return {"node_id": node["id"], "kind": "parallel", "children": list(results)}


async def _execute_fan_in(node: dict[str, Any], state: GraphExecutionState) -> dict[str, Any]:
    results = await asyncio.gather(*(execute_graph(child, state) for child in node.get("children", [])))
    merged = [r.get("result", r) for r in results]
    return {"node_id": node["id"], "kind": "fan_in", "merged": merged, "children": list(results)}


async def _execute_conditional(node: dict[str, Any], state: GraphExecutionState) -> dict[str, Any]:
    condition = node["condition"]
    referenced = state.context.get(condition["node_ref"], _MISSING)
    taken_true = referenced is not _MISSING and _condition_matches(referenced, condition)
    branch = node["if_true"] if taken_true else node.get("if_false")

    if branch is None:
        return {"node_id": node["id"], "kind": "conditional", "taken": "none", "branch": None}

    result = await execute_graph(branch, state)
    return {
        "node_id": node["id"],
        "kind": "conditional",
        "taken": "if_true" if taken_true else "if_false",
        "branch": result,
    }


async def _execute_loop(node: dict[str, Any], state: GraphExecutionState) -> dict[str, Any]:
    max_iterations = node["max_iterations"]
    if max_iterations < 1:
        raise GraphError(f"loop node {node['id']!r}: max_iterations must be >= 1")
    stop_condition = node.get("stop_condition")

    iterations = []
    stopped_reason = "max_iterations"
    for i in range(max_iterations):
        body = dict(node["body"])
        body["id"] = f"{node['body']['id']}[{i}]"
        iteration_result = await execute_graph(body, state)
        iterations.append(iteration_result)
        if stop_condition and _condition_matches(iteration_result, stop_condition):
            stopped_reason = "condition_met"
            break

    return {"node_id": node["id"], "kind": "loop", "iterations": iterations, "stopped_reason": stopped_reason}


async def _execute_subgraph(node: dict[str, Any], state: GraphExecutionState) -> dict[str, Any]:
    # Wraps the inner graph's result rather than returning it verbatim: every other kind wraps its
    # children's results under its own node_id, and a "transparent" subgraph would make the outer
    # node's own id unfindable in the result tree (state.context[node_id] would record a dict whose
    # own "node_id" field is the INNER graph's, not this subgraph node's).
    inner_result = await execute_graph(node["graph"], state)
    return {"node_id": node["id"], "kind": "subgraph", "graph_result": inner_result}
