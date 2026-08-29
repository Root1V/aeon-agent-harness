"""test_approval_binding — the acceptance test named in roadmap.md RUN-005.

Spec §9 acceptance criterion: "una acción irreversible sin aprobación válida queda bloqueada y el
run se serializa/reanuda tras la decisión."

Runs a real GraphRunWorkflow, with a single tool_call node marked requires_approval, against a real
ephemeral Temporal server, and drives it through Temporal signals exactly as a Run Controller would
(no HTTP layer needed for this test — go/internal/api's RunControllerHandlers is a thin wrapper
over the same signal/query primitives exercised here directly). Four scenarios, one per outcome:
approved (with the exact bound hash), rejected, approved with a MISMATCHED hash (parameter binding
— the core of this feature), and expired (TTL elapses with no decision at all). In every
non-approved case, the tool_call must never actually run.
"""
from __future__ import annotations

import uuid

import pytest
from temporalio.client import Client, WorkflowFailureError
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from aeon_worker.activities.tool_activities import execute_tool_activity
from aeon_worker.graph import compute_tool_call_hash
from aeon_worker.workflows.graph_run import GraphRunWorkflow

NODE_ID = "n0"
TOOL_NAME = "artifact.write"
TOOL_ARGS = {"path": "irreversible.txt"}
REAL_HASH = compute_tool_call_hash(NODE_ID, TOOL_NAME, TOOL_ARGS)


def approval_graph() -> dict:
    return {"id": NODE_ID, "kind": "tool_call", "tool_name": TOOL_NAME, "tool_args": TOOL_ARGS, "requires_approval": True}


async def _start(env: WorkflowEnvironment, worker: Worker, run_id: str, request_extra: dict | None = None):
    request = {"run_id": run_id, "graph": approval_graph()}
    if request_extra:
        request.update(request_extra)
    return await env.client.start_workflow(
        GraphRunWorkflow.run, request, id=f"graph-run-{run_id}", task_queue=worker.task_queue
    )


async def _wait_for_pending_approval(handle) -> dict:
    """The workflow blocks inside _await_approval before the tool_call ever runs; poll the
    pending_approval query until it appears (it's set synchronously before the wait, so this
    resolves on the first or second poll in practice)."""
    for _ in range(50):
        pending = await handle.query(GraphRunWorkflow.pending_approval)
        if pending is not None:
            return pending
        await __import__("asyncio").sleep(0.05)
    raise AssertionError("pending_approval never appeared")


@pytest.mark.asyncio
async def test_approval_binding_approved_runs_the_call():
    run_id = str(uuid.uuid4())
    task_queue = f"aeon-approval-test-{uuid.uuid4().hex[:8]}"
    async with await WorkflowEnvironment.start_local() as env:
        async with Worker(env.client, task_queue=task_queue, workflows=[GraphRunWorkflow], activities=[execute_tool_activity]) as worker:
            handle = await _start(env, worker, run_id)
            pending = await _wait_for_pending_approval(handle)
            assert pending["tool_call_hash"] == REAL_HASH

            await handle.signal(GraphRunWorkflow.approve, {"approval_id": pending["approval_id"], "tool_call_hash": pending["tool_call_hash"]})
            outcome = await handle.result()

    assert outcome["result"]["result"]["status"] == "written"


@pytest.mark.asyncio
async def test_approval_binding_rejected_never_runs():
    run_id = str(uuid.uuid4())
    task_queue = f"aeon-approval-test-{uuid.uuid4().hex[:8]}"
    async with await WorkflowEnvironment.start_local() as env:
        async with Worker(env.client, task_queue=task_queue, workflows=[GraphRunWorkflow], activities=[execute_tool_activity]) as worker:
            handle = await _start(env, worker, run_id)
            pending = await _wait_for_pending_approval(handle)

            await handle.signal(GraphRunWorkflow.reject, {"approval_id": pending["approval_id"], "tool_call_hash": pending["tool_call_hash"]})
            with pytest.raises(WorkflowFailureError) as exc_info:
                await handle.result()
            consumed = await handle.query(GraphRunWorkflow.budgets_consumed)

    assert "rejected" in str(exc_info.value.cause)
    assert consumed["tool_calls"] == 0, "a rejected call must never execute"


@pytest.mark.asyncio
async def test_approval_binding_mismatched_hash_is_denied():
    """The core of RUN-005's parameter binding: a decision naming a DIFFERENT tool_call_hash than
    the one actually pending must be denied — an approval only applies to the exact parameters it
    was granted for, matching the spec's "aprobar con args A, mutar a B antes de ejecutar ->
    rechazo"."""
    run_id = str(uuid.uuid4())
    task_queue = f"aeon-approval-test-{uuid.uuid4().hex[:8]}"
    async with await WorkflowEnvironment.start_local() as env:
        async with Worker(env.client, task_queue=task_queue, workflows=[GraphRunWorkflow], activities=[execute_tool_activity]) as worker:
            handle = await _start(env, worker, run_id)
            pending = await _wait_for_pending_approval(handle)

            stale_hash = compute_tool_call_hash(NODE_ID, TOOL_NAME, {"path": "different-args.txt"})
            assert stale_hash != pending["tool_call_hash"]
            await handle.signal(GraphRunWorkflow.approve, {"approval_id": pending["approval_id"], "tool_call_hash": stale_hash})

            with pytest.raises(WorkflowFailureError) as exc_info:
                await handle.result()
            consumed = await handle.query(GraphRunWorkflow.budgets_consumed)

    assert "different tool_call_hash" in str(exc_info.value.cause)
    assert consumed["tool_calls"] == 0, "an approval bound to different parameters must never execute the call"


@pytest.mark.asyncio
async def test_approval_binding_expires_without_decision():
    run_id = str(uuid.uuid4())
    task_queue = f"aeon-approval-test-{uuid.uuid4().hex[:8]}"
    async with await WorkflowEnvironment.start_local() as env:
        async with Worker(env.client, task_queue=task_queue, workflows=[GraphRunWorkflow], activities=[execute_tool_activity]) as worker:
            # A 1-second TTL and no signal at all — the call must never run.
            handle = await _start(env, worker, run_id, {"approvals": {"default_ttl_seconds": 1}})
            with pytest.raises(WorkflowFailureError) as exc_info:
                await handle.result()
            consumed = await handle.query(GraphRunWorkflow.budgets_consumed)

    assert "expired" in str(exc_info.value.cause)
    assert consumed["tool_calls"] == 0, "an expired approval must never execute the call"
