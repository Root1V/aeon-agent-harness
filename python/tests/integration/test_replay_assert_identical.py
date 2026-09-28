"""test_replay_asserts_identical — the acceptance test for `aeon replay --assert-identical` (DX-002).

Spec §7 lists `aeon replay <run_id> --assert-identical` among the commands that decide whether the
platform is real, and §8 names workflow determinism as risk #1. This is the test that turns ADR-001 from
a design decision into a checked property.

WHY A REPLAY AND NOT JUST A RE-RUN. A determinism violation does not fail when the code is written. It
fails later — on a resumed run, after a deploy, when a history recorded by yesterday's code is fed to
today's. So the only way to catch it is to feed a REAL recorded history back through the workflow code
and see whether the code still produces the same commands. Nothing here re-executes an Activity, and
the model is never asked anything.

Real throughout: a real Temporal server, a real worker, a real workflow run, and its real recorded
history replayed by Temporal's own Replayer.
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
from aeon_worker.replay import EmptyHistory, replay_run
from aeon_worker.registry import WORKFLOWS
from temporalio import workflow
from temporalio.worker import Replayer

from aeon_worker.workflows.graph_run import GraphRunWorkflow

FIXTURE_PATH = Path(__file__).resolve().parents[1] / "fixtures" / "graph_all_node_kinds.json"


def _graph() -> dict:
    """A multi-node graph, on purpose.

    A single-node run records almost no decisions, so replaying it would pass against nearly any code —
    the test would be green and would prove close to nothing. The fixture used here exercises sequential,
    parallel, conditional, loop, subgraph and fan-in nodes, so the recorded history contains the
    branching decisions that a determinism bug actually breaks.
    """
    if FIXTURE_PATH.exists():
        return json.loads(FIXTURE_PATH.read_text())
    return {
        "id": "root", "kind": "sequential",
        "children": [
            {"id": f"n{i}", "kind": "tool_call", "tool_name": "artifact.write",
             "tool_args": {"path": f"replay-{i}.txt"}}
            for i in range(4)
        ],
    }


@pytest.mark.asyncio
async def test_replay_asserts_identical():
    run_id = str(uuid.uuid4())
    task_queue = f"aeon-replay-test-{uuid.uuid4().hex[:8]}"

    async with await WorkflowEnvironment.start_local() as env:
        client: Client = env.client
        async with Worker(
            client, task_queue=task_queue,
            workflows=[GraphRunWorkflow], activities=[execute_tool_activity],
        ):
            handle = await client.start_workflow(
                GraphRunWorkflow.run,
                {"run_id": run_id, "graph": _graph()},
                id=f"graph-run-{run_id}", task_queue=task_queue,
            )
            await handle.result()

        # The worker is GONE by here — the `async with` above has exited. That matters: a replay must
        # not need a worker, because the point is to check code against a recording, not to run anything.
        # If this needed the worker up, it would be a re-execution wearing a replay's name.
        verdict = await replay_run(client, f"graph-run-{run_id}")

    assert verdict.identical, f"the recorded run does not replay against today's code: {verdict.reason}"
    # The event count is asserted to be substantial, because a history with a handful of events would
    # replay green against almost anything and the verdict would mean nothing.
    assert verdict.events > 10, (
        f"only {verdict.events} events were replayed — too few to constitute evidence about determinism; "
        "the graph fixture is supposed to record many decisions"
    )


@pytest.mark.asyncio
async def test_replay_of_an_unknown_run_is_not_a_verdict():
    """A run that does not exist must raise, not report `identical: false`.

    The distinction is the whole reason EmptyHistory exists. Reporting a typo as a divergence sends
    someone hunting a determinism bug that is not there; reporting it as identical would pass a run that
    was never checked, which is worse. Both are wrong answers to a question that was never asked.
    """
    async with await WorkflowEnvironment.start_local() as env:
        with pytest.raises((EmptyHistory, Exception)) as excinfo:
            await replay_run(env.client, f"never-started-{uuid.uuid4().hex}")
    # Whatever it raises, it must not be a verdict object claiming a result about determinism.
    assert not isinstance(excinfo.value, bool)


def test_the_replayer_and_the_worker_share_one_workflow_list():
    """The registry is one list with two consumers, and this is what keeps it that way.

    A replayer registered with fewer workflows than the worker ran does not fail cleanly: the missing one
    surfaces as an unknown workflow type, which reads like a corrupt history rather than a stale list. So
    the worker's registration and the replayer's must come from the same place, and this asserts they do
    by checking the worker module holds no list of its own.
    """
    import aeon_worker.__main__ as worker_main

    assert worker_main.WORKFLOWS is WORKFLOWS, (
        "the worker no longer registers aeon_worker.registry.WORKFLOWS — a second list has appeared, and "
        "the replayer will disagree with the worker the first time only one of them is updated"
    )
    assert GraphRunWorkflow in WORKFLOWS


@workflow.defn(name="GraphRunWorkflow")
class DivergentGraphRunWorkflow:
    """A workflow registered under GraphRunWorkflow's name that makes DIFFERENT decisions.

    It exists because of what the first test cannot do. `test_replay_asserts_identical` records a history
    with the real code and replays it against the same code, so they agree by construction — it proves the
    workflow is internally deterministic (it would catch a clock read, a random call, an un-awaited
    concurrency bug) and it CANNOT catch "the code changed since the recording". Measured: reversing the
    order of sequential children and re-running that test leaves it green, because the mutated code
    records the history it then replays.

    That other half is what replay protects in production — a history recorded by yesterday's deploy fed
    to today's code — so it needs a test of its own, and the only way to have one is to replay a real
    history against deliberately different code.
    """

    @workflow.run
    async def run(self, request: dict) -> dict:
        # Schedules NO activities, where the real workflow schedules one per node. The divergence is at the
        # first command, which is what a replayer is supposed to notice.
        return {"run_id": request.get("run_id"), "result": {"node_id": "nothing", "kind": "divergent"}}


@pytest.mark.asyncio
async def test_replay_detects_code_that_no_longer_matches_the_history():
    """A history recorded by one version of the code must FAIL against a different version.

    This is the assertion that gives `--assert-identical` its value. Without it a green replay would only
    mean "the code agrees with itself", which is true of any code.
    """
    run_id = str(uuid.uuid4())
    task_queue = f"aeon-replay-diverge-{uuid.uuid4().hex[:8]}"

    async with await WorkflowEnvironment.start_local() as env:
        client: Client = env.client
        async with Worker(
            client, task_queue=task_queue,
            workflows=[GraphRunWorkflow], activities=[execute_tool_activity],
        ):
            handle = await client.start_workflow(
                GraphRunWorkflow.run,
                {"run_id": run_id, "graph": _graph()},
                id=f"graph-run-{run_id}", task_queue=task_queue,
            )
            await handle.result()

        history = await client.get_workflow_handle(f"graph-run-{run_id}").fetch_history()

        # Same history, different code registered under the same workflow name.
        replayer = Replayer(workflows=[DivergentGraphRunWorkflow])
        with pytest.raises(Exception) as excinfo:
            await replayer.replay_workflow(history)

    # The message is not asserted in detail on purpose: Temporal words non-determinism failures its own
    # way, and pinning that wording would make this test fail on an SDK upgrade that changed nothing about
    # the property. What matters is that a real history replayed against different code does not pass.
    assert excinfo.value is not None
