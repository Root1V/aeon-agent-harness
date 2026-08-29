"""test_graph_runtime_node_kinds — the acceptance test named in roadmap.md RUN-002.

Runs tests/fixtures/graph_all_node_kinds.json (the same fixture test_contracts.py schema-validates)
through GraphRunWorkflow against a REAL ephemeral Temporal server, exercising all six GraphNode
kinds in one tree: sequential (root + the subgraph's inner graph), parallel, conditional (branch
taken based on a sibling's already-executed result), loop (once stopping early via stop_condition,
once running to max_iterations), subgraph, and fan_in.

Unlike test_crash_resume.py, this test needs no subprocess/crash choreography — it uses an
in-process Worker, since there's nothing here that must survive a real process death.
"""
from __future__ import annotations

import json
import uuid
from pathlib import Path

import pytest
from temporalio.client import Client
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from aeon_worker.activities.tool_activities import execute_tool_activity
from aeon_worker.workflows.graph_run import GraphRunWorkflow

FIXTURE_PATH = Path(__file__).resolve().parents[1] / "fixtures" / "graph_all_node_kinds.json"


def _by_id(node_result: dict, target_id: str) -> dict:
    """Depth-first search through a GraphRunWorkflow result tree for the node with id==target_id."""
    if node_result.get("node_id") == target_id:
        return node_result
    for key in ("children", "iterations"):
        for child in node_result.get(key, []) or []:
            found = _by_id(child, target_id)
            if found is not None:
                return found
    for key in ("branch", "graph_result"):
        child = node_result.get(key)
        if isinstance(child, dict):
            found = _by_id(child, target_id)
            if found is not None:
                return found
    return None


@pytest.mark.asyncio
async def test_graph_runtime_node_kinds():
    graph = json.loads(FIXTURE_PATH.read_text())
    run_id = str(uuid.uuid4())
    task_queue = f"aeon-graph-test-{uuid.uuid4().hex[:8]}"

    async with await WorkflowEnvironment.start_local() as env:
        client: Client = env.client
        async with Worker(
            client,
            task_queue=task_queue,
            workflows=[GraphRunWorkflow],
            activities=[execute_tool_activity],
        ):
            handle = await client.start_workflow(
                GraphRunWorkflow.run,
                {"run_id": run_id, "graph": graph},
                id=f"graph-run-{run_id}",
                task_queue=task_queue,
            )
            outcome = await handle.result()

    assert outcome["run_id"] == run_id
    root = outcome["result"]
    assert root["node_id"] == "root"
    assert root["kind"] == "sequential"
    # sequential ran every top-level child, in declaration order.
    assert [c["node_id"] for c in root["children"]] == [
        "greet", "parallel-block", "branch-on-p1", "loop-stops-early", "loop-runs-full",
        "wrapped-subgraph", "fan-in-block",
    ]

    # parallel: both branches actually executed (not skipped).
    parallel_block = _by_id(root, "parallel-block")
    assert {c["node_id"] for c in parallel_block["children"]} == {"p1", "p2"}
    assert all(c["result"]["status"] == "written" for c in parallel_block["children"])

    # conditional: p1 wrote status="written", so the condition matched and if_true was taken —
    # if_false ("cond-false") must NOT appear anywhere in the tree.
    conditional = _by_id(root, "branch-on-p1")
    assert conditional["taken"] == "if_true"
    assert conditional["branch"]["node_id"] == "cond-true"
    assert _by_id(root, "cond-false") is None

    # loop-stops-early: stop_condition matches after the very first iteration (every write
    # returns status="written"), so it must NOT run all 5 allowed iterations.
    loop_early = _by_id(root, "loop-stops-early")
    assert loop_early["stopped_reason"] == "condition_met"
    assert len(loop_early["iterations"]) == 1

    # loop-runs-full: stop_condition never matches, so it must exhaust max_iterations=3.
    loop_full = _by_id(root, "loop-runs-full")
    assert loop_full["stopped_reason"] == "max_iterations"
    assert len(loop_full["iterations"]) == 3
    # each iteration got a distinct node id / idempotency key despite identical tool_args.
    iter_ids = [it["node_id"] for it in loop_full["iterations"]]
    assert iter_ids == ["loop-runs-full-body[0]", "loop-runs-full-body[1]", "loop-runs-full-body[2]"]
    assert len({it["idempotency_key"] for it in loop_full["iterations"]}) == 3

    # subgraph: wraps the inner graph's result under graph_result, so both the subgraph node's own
    # id and the inner graph's id/kind remain independently findable.
    subgraph = _by_id(root, "wrapped-subgraph")
    assert subgraph["kind"] == "subgraph"
    assert subgraph["graph_result"]["node_id"] == "inner-sequential"
    assert subgraph["graph_result"]["kind"] == "sequential"
    assert _by_id(root, "inner-node")["result"]["status"] == "written"

    # fan_in: both children ran, and their results were merged into a single flat list.
    fan_in = _by_id(root, "fan-in-block")
    assert {c["node_id"] for c in fan_in["children"]} == {"f1", "f2"}
    assert len(fan_in["merged"]) == 2
    assert all(m["status"] == "written" for m in fan_in["merged"])
