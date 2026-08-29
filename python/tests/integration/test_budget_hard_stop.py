"""test_budget_hard_stop — the acceptance test named in roadmap.md RUN-003.

Runs a real GraphRunWorkflow against a real ephemeral Temporal server three times, once per budget
dimension the Graph Runtime can actually enforce today (tool_calls, depth, deadline — model_calls/
tokens/cost_usd aren't enforced yet, since there's no Model Gateway call site to count them from,
MDL-001 still TODO). Each scenario proves the HARD stop: the limit-crossing action never runs at
all, not just that the run ends up failed.
"""
from __future__ import annotations

import uuid

import pytest
from temporalio.client import Client, WorkflowFailureError
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from aeon_worker.activities.tool_activities import execute_tool_activity
from aeon_worker.workflows.graph_run import GraphRunWorkflow


def _tool_call(node_id: str, path: str) -> dict:
    return {"id": node_id, "kind": "tool_call", "tool_name": "artifact.write", "tool_args": {"path": path}}


async def _run_and_expect_budget_failure(env: WorkflowEnvironment, graph: dict, budgets: dict) -> tuple[str, dict]:
    """Starts the workflow, expects it to fail, and returns (failure_message, budgets_consumed) —
    budgets_consumed is queried from the now-closed workflow, which Temporal answers by replaying
    history."""
    run_id = str(uuid.uuid4())
    task_queue = f"aeon-budget-test-{uuid.uuid4().hex[:8]}"
    client: Client = env.client
    async with Worker(client, task_queue=task_queue, workflows=[GraphRunWorkflow], activities=[execute_tool_activity]):
        handle = await client.start_workflow(
            GraphRunWorkflow.run,
            {"run_id": run_id, "graph": graph, "budgets": budgets},
            id=f"graph-run-{run_id}",
            task_queue=task_queue,
        )
        with pytest.raises(WorkflowFailureError) as exc_info:
            await handle.result()
        consumed = await handle.query(GraphRunWorkflow.budgets_consumed)
        return str(exc_info.value.cause), consumed


@pytest.mark.asyncio
async def test_budget_hard_stop_tool_calls():
    # A loop willing to run 10 iterations, but the budget only allows 3 — the loop must not be
    # allowed to reach iteration 4.
    graph = {
        "id": "root", "kind": "loop", "max_iterations": 10,
        "body": {"id": "body", "kind": "tool_call", "tool_name": "artifact.write", "tool_args": {"path": "x.txt"}},
    }
    async with await WorkflowEnvironment.start_local() as env:
        message, consumed = await _run_and_expect_budget_failure(env, graph, {"max_tool_calls": 3})

    assert "tool_calls_exceeded" in message
    assert consumed["tool_calls"] == 3, f"expected exactly 3 tool calls to have run, consumed={consumed}"


@pytest.mark.asyncio
async def test_budget_hard_stop_depth():
    # Two levels of subgraph nesting, but max_depth only allows one.
    graph = {
        "id": "root", "kind": "subgraph",
        "graph": {
            "id": "level1", "kind": "subgraph",
            "graph": _tool_call("level2-node", "too-deep.txt"),
        },
    }
    async with await WorkflowEnvironment.start_local() as env:
        message, consumed = await _run_and_expect_budget_failure(env, graph, {"max_depth": 1})

    assert "depth_exceeded" in message
    # The node at depth 2 must never have run — only up to depth 1 was ever reached.
    assert consumed["tool_calls"] == 0, f"expected the too-deep tool_call to never run, consumed={consumed}"


@pytest.mark.asyncio
async def test_budget_hard_stop_deadline():
    # A deadline of 0 seconds is already in the past by the time the workflow's first task runs —
    # the graph's only node must never execute.
    graph = _tool_call("n0", "never-runs.txt")
    async with await WorkflowEnvironment.start_local() as env:
        message, consumed = await _run_and_expect_budget_failure(env, graph, {"deadline_seconds": 0})

    assert "deadline_exceeded" in message
    assert consumed["tool_calls"] == 0, f"expected the node to never run past the deadline, consumed={consumed}"
