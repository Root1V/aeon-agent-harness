"""VRT-AEON-001: a run executes steps on a worker Aeon does not own, under Aeon's governance.

Veritium's five acceptance criteria, each as its own test, plus the budget counter. The external
worker is a SECOND, REAL Temporal worker on its own task queue (`test-external`) — not a double and
not the same worker with a different name, because what the feature claims is precisely that the
work is picked up by a different process on a different queue.

The policy check is a real HTTP call to a real aeon-toolgw with the real shipped Cedar bundle, so
these tests self-skip without AEON_TEST_TOOLGW_ADDR. That is the point rather than a limitation: a
double there would verify the request shape and nothing about authorization, which is the half of
this feature that decides whether a run may hand work outside at all.
"""
from __future__ import annotations

import asyncio
import os
import uuid

import pytest
from temporalio import activity
from temporalio.client import Client, WorkflowFailureError
from temporalio.exceptions import ApplicationError
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from aeon_worker.activities.activity_policy_activities import check_activity_policy_activity
from aeon_worker.activities.tool_activities import execute_tool_activity
from aeon_worker.replay import replay_run
from aeon_worker.workflows.graph_run import GraphRunWorkflow

TOOLGW_ADDR = os.environ.get("AEON_TEST_TOOLGW_ADDR", "")
EXTERNAL_QUEUE = "test-external"
# The agent the shipped bundle's `allow-test-external-activity` permit names. It has no manifest in
# the registry on purpose, so the permit cannot widen any real run.
TEST_AGENT = "activity-node-test@0.1.0"
ACTIVITY_NAME = "aeon.test.external_activity"

pytestmark = pytest.mark.skipif(
    not TOOLGW_ADDR,
    reason="AEON_TEST_TOOLGW_ADDR not set — the activity node's policy check is a real call to a real "
    "Cedar bundle, and a double would verify the request shape and nothing about authorization",
)


@pytest.fixture(autouse=True)
def _point_the_policy_check_at_the_test_gateway(monkeypatch):
    """Bridge AEON_TEST_TOOLGW_ADDR to the production variable the activity reads, FOR THESE TESTS ONLY.

    THE FIRST VERSION DID THIS AT IMPORT TIME AND BROKE FOUR OTHER TESTS. pytest imports every test
    module before running any of them, so a module-level `os.environ[...] = ...` is a process-wide
    change: `test_deep_research_workflow` runs with AEON_TOOL_EXECUTION_MODE=local-ledger, saw both
    variables set, and `tool_activities` refused — "both AEON_TOOLGW_ADDR and
    AEON_TOOL_EXECUTION_MODE=local-ledger are set: these select different execution backends and only
    one can be meant."

    Which is the guard working exactly as intended, and the joke is on the comment I had written
    there: it said the bridge lived in the test file rather than the Makefile so it would not widen
    the whole target, and it widened the whole target by a different door. monkeypatch undoes it per
    test, which is the only scope that was ever meant.
    """
    if TOOLGW_ADDR:
        monkeypatch.setenv("AEON_TOOLGW_ADDR", TOOLGW_ADDR)


class ExternalWorker:
    """A real worker on a real second task queue, counting what it is actually asked to run."""

    def __init__(self) -> None:
        self.calls: list[dict] = []
        self.fail_with: Exception | None = None
        # Items held until released, so a test can park ONE activity in flight instead of racing a
        # poll against a worker that finishes in microseconds.
        self.held: dict[str, asyncio.Event] = {}

    def hold(self, item: str) -> None:
        self.held[item] = asyncio.Event()

    def release(self, item: str) -> None:
        if item in self.held:
            self.held[item].set()

    def ran(self, item: str) -> int:
        return sum(1 for c in self.calls if c.get("item_id") == item)

    def activities(self):
        counter = self

        @activity.defn(name=ACTIVITY_NAME)
        async def external(args: dict) -> dict:
            counter.calls.append(args)
            gate = counter.held.get(args.get("item_id", ""))
            if gate is not None:
                await gate.wait()
            if counter.fail_with is not None:
                raise counter.fail_with
            return {"processed": args.get("item_id"), "attempt": len(counter.calls)}

        return [external]


def _why(exc: BaseException) -> str:
    """Every message in the cause chain, joined.

    `str(WorkflowFailureError)` is the literal string "Workflow execution failed" and nothing else —
    the reason lives in .cause (an ActivityError) and .cause.cause (the ApplicationError the activity
    raised). Asserting on str() therefore passes for ANY failure, which is how a test that looks
    specific stops being it.
    """
    parts, seen = [], 0
    cur: BaseException | None = exc
    while cur is not None and seen < 10:
        parts.append(f"{type(cur).__name__}: {cur}")
        cur = cur.__cause__ or getattr(cur, "cause", None)
        seen += 1
    return " | ".join(parts)


def _graph_with(*nodes: dict) -> dict:
    return {"id": "root", "kind": "sequential", "children": list(nodes)}


def _activity_node(node_id: str, item: str, **extra) -> dict:
    node = {
        "id": node_id,
        "kind": "activity",
        "activity_name": ACTIVITY_NAME,
        "task_queue": EXTERNAL_QUEUE,
        "args": {"item_id": item},
    }
    node.update(extra)
    return node


async def _start(client: Client, queue: str, graph: dict, **request):
    run_id = f"activity-node-{uuid.uuid4().hex[:8]}"
    payload = {"run_id": run_id, "graph": graph, "agent_manifest_ref": TEST_AGENT}
    payload.update(request)
    handle = await client.start_workflow(
        GraphRunWorkflow.run, payload, id=f"graph-run-{run_id}", task_queue=queue
    )
    return handle


def _by_id(result: dict, target: str) -> dict | None:
    if result.get("node_id") == target:
        return result
    for key in ("children", "iterations"):
        for child in result.get(key, []) or []:
            found = _by_id(child, target)
            if found is not None:
                return found
    for key in ("branch", "graph_result"):
        child = result.get(key)
        if isinstance(child, dict):
            found = _by_id(child, target)
            if found is not None:
                return found
    return None


@pytest.mark.asyncio
async def test_parallel_activities_fan_in_then_one_more():
    """Criterion 1: parallel[activity x 3] -> fan_in -> activity, on another task queue."""
    external = ExternalWorker()
    graph = _graph_with(
        {
            "id": "fan",
            "kind": "fan_in",
            "children": [_activity_node(f"p{i}", f"item-{i}") for i in range(3)],
        },
        _activity_node("after", "summary"),
    )

    async with await WorkflowEnvironment.start_local() as env:
        aeon_queue = f"aeon-{uuid.uuid4().hex[:8]}"
        async with Worker(
            env.client, task_queue=aeon_queue, workflows=[GraphRunWorkflow],
            activities=[execute_tool_activity, check_activity_policy_activity],
        ), Worker(env.client, task_queue=EXTERNAL_QUEUE, activities=external.activities()):
            handle = await _start(env.client, aeon_queue, graph)
            result = (await handle.result())["result"]

            consumed = await handle.query(GraphRunWorkflow.budgets_consumed)

            # CRITERION 5, against the production replayer and not a reimplementation of it:
            # aeon_worker.replay is what `aeon replay --assert-identical` points a person at, run
            # with the same interceptor list the worker uses — a replayer configured differently is
            # not replaying what the worker did.
            #
            # This is the check that a NEW NODE KIND most needs and least obviously passes, because
            # scheduling an activity adds commands to the history. It passes here for a reason worth
            # stating: the `activity` branch is reachable only from the graph JSON, which is part of
            # the run's input and fixed at start, so this history replays against today's code and a
            # history written before this kind existed never reaches the new branch at all. That is
            # the difference from OBS-010, where an Activity was added to a path in-flight runs DID
            # traverse and `workflow.patched` was the only way through.
            verdict = await replay_run(env.client, handle.id)

    assert len(external.calls) == 4, (
        f"the external worker ran {len(external.calls)} activities, want 4 — the three parallel "
        "children plus the one after the fan_in"
    )
    assert sorted(c["item_id"] for c in external.calls) == ["item-0", "item-1", "item-2", "summary"]

    merged = _by_id(result, "fan")["merged"]
    assert len(merged) == 3, f"fan_in merged {merged!r}"
    after = _by_id(result, "after")
    assert after["result"]["processed"] == "summary"
    assert after["task_queue"] == EXTERNAL_QUEUE
    assert after["policy_id"], "the result does not name the policy that permitted handing work outside"

    # The counter is its own, not folded into tool_calls — a run that called no tool must report zero.
    assert verdict.identical, f"replay of a run with activity nodes is not identical: {verdict.reason}"
    assert verdict.events > 0

    assert consumed["activity_calls"] == 4, consumed
    assert consumed["tool_calls"] == 0, (
        f"tool_calls = {consumed['tool_calls']}: external activities are being counted as tool calls, "
        "which is the one-counter-two-meanings defect MDL-018 removed from this same dict"
    )


@pytest.mark.asyncio
async def test_a_non_retryable_error_is_not_retried():
    """Criterion 2: maximum_attempts and non_retryable_errors behave as Temporal's."""
    external = ExternalWorker()
    external.fail_with = ApplicationError("the item is incomplete", type="ItemIncomplete", non_retryable=False)
    graph = _graph_with(
        _activity_node(
            "step", "bad",
            timeout_seconds=600,
            retry={"maximum_attempts": 5, "initial_interval_seconds": 1,
                   "non_retryable_errors": ["ItemIncomplete"]},
        )
    )

    async with await WorkflowEnvironment.start_local() as env:
        aeon_queue = f"aeon-{uuid.uuid4().hex[:8]}"
        async with Worker(
            env.client, task_queue=aeon_queue, workflows=[GraphRunWorkflow],
            activities=[execute_tool_activity, check_activity_policy_activity],
        ), Worker(env.client, task_queue=EXTERNAL_QUEUE, activities=external.activities()):
            handle = await _start(env.client, aeon_queue, graph)
            with pytest.raises(WorkflowFailureError):
                await handle.result()

    # THE NUMBER IS THE ASSERTION. maximum_attempts=5 would have run this five times; the error type
    # is in non_retryable_errors, so it runs once. If the node's retry policy were ignored and the
    # hardcoded tool_call policy applied instead, this would be 5.
    assert len(external.calls) == 1, (
        f"the external worker ran {len(external.calls)} times for a non-retryable error type — "
        "non_retryable_errors from the node is not reaching Temporal"
    )


@pytest.mark.asyncio
async def test_a_completed_activity_is_not_re_executed_when_the_worker_comes_back():
    """Criterion 3: the external worker goes away mid-run and the run resumes without repeating
    work that already happened.

    THE RACE THE FIRST VERSION OF THIS TEST HAD: it polled for "one activity has run" and then shut
    the worker down, but the worker finishes in microseconds, so both nodes were done before the
    poll looked — and the test failed with `assert 2 == 1` on a feature that was working. The second
    item is HELD now, so exactly one activity is complete and exactly one is in flight when the
    worker disappears. A test that controls the state it is about beats a test that waits for it.
    """
    external = ExternalWorker()
    external.hold("two")
    # Short timeout on purpose: a worker that vanishes leaves the task unreported, so Temporal only
    # re-delivers after start_to_close elapses. The 600s default would make this test take ten
    # minutes to assert something that happens immediately.
    graph = _graph_with(
        _activity_node("first", "one", timeout_seconds=5,
                       retry={"maximum_attempts": 5, "initial_interval_seconds": 1}),
        _activity_node("second", "two", timeout_seconds=5,
                       retry={"maximum_attempts": 5, "initial_interval_seconds": 1}),
    )

    async with await WorkflowEnvironment.start_local() as env:
        aeon_queue = f"aeon-{uuid.uuid4().hex[:8]}"
        async with Worker(
            env.client, task_queue=aeon_queue, workflows=[GraphRunWorkflow],
            activities=[execute_tool_activity, check_activity_policy_activity],
        ):
            handle = await _start(env.client, aeon_queue, graph)

            async with Worker(env.client, task_queue=EXTERNAL_QUEUE, activities=external.activities()):
                for _ in range(200):
                    if external.ran("two") >= 1:
                        break
                    await asyncio.sleep(0.05)
            # One complete, one in flight and held when the worker went away.
            assert external.ran("one") == 1, f"calls: {external.calls}"
            assert external.ran("two") == 1, f"calls: {external.calls}"

            # No worker on that queue. The run is parked on a task Temporal is holding, which is the
            # durability this node exists for.
            await asyncio.sleep(0.5)
            assert len(external.calls) == 2

            external.release("two")
            async with Worker(env.client, task_queue=EXTERNAL_QUEUE, activities=external.activities()):
                result = (await handle.result())["result"]

    # THE TWO NUMBERS ARE THE ASSERTION, and they differ on purpose. "one" COMPLETED before the
    # worker died, so its result is in the history and it is never asked for again — that is the "sin
    # re-ejecutar las actividades ya completadas" half. "two" was IN FLIGHT and unreported, so
    # Temporal correctly re-delivers it: an interrupted attempt is retried, and a node whose effect
    # must not happen twice is what idempotency on the consumer's side is for.
    assert external.ran("one") == 1, (
        f"the completed activity ran {external.ran('one')} times — a finished effect happened again "
        f"when the worker returned. calls: {external.calls}"
    )
    assert external.ran("two") == 2, f"calls: {external.calls}"
    assert _by_id(result, "second")["result"]["processed"] == "two"


@pytest.mark.asyncio
async def test_cedar_denies_an_activity_name_that_is_not_permitted():
    """Criterion 4, first half: policy can refuse, and the work never reaches the worker."""
    external = ExternalWorker()
    graph = _graph_with(
        {
            "id": "forbidden",
            "kind": "activity",
            # Named exactly like a tool the bundle permits. The permit says `resource is Tool`, so an
            # ExternalActivity must not inherit it — the hole A2A-002 measured, one resource kind on.
            "activity_name": "search.web",
            "task_queue": EXTERNAL_QUEUE,
            "args": {},
        }
    )

    async with await WorkflowEnvironment.start_local() as env:
        aeon_queue = f"aeon-{uuid.uuid4().hex[:8]}"
        async with Worker(
            env.client, task_queue=aeon_queue, workflows=[GraphRunWorkflow],
            activities=[execute_tool_activity, check_activity_policy_activity],
        ), Worker(env.client, task_queue=EXTERNAL_QUEUE, activities=external.activities()):
            handle = await _start(env.client, aeon_queue, graph)
            with pytest.raises(WorkflowFailureError) as excinfo:
                await handle.result()

    assert "denied by policy" in _why(excinfo.value), _why(excinfo.value)
    assert external.calls == [], (
        "the external worker ran the activity anyway — a denial that arrives after the work is a "
        "report, not a boundary"
    )


@pytest.mark.asyncio
async def test_requires_approval_blocks_until_a_person_says_yes():
    """Criterion 4, second half: the gate is durable and the work does not happen while it is open."""
    external = ExternalWorker()
    graph = _graph_with(_activity_node("gated", "needs-a-person", requires_approval=True))

    async with await WorkflowEnvironment.start_local() as env:
        aeon_queue = f"aeon-{uuid.uuid4().hex[:8]}"
        async with Worker(
            env.client, task_queue=aeon_queue, workflows=[GraphRunWorkflow],
            activities=[execute_tool_activity, check_activity_policy_activity],
        ), Worker(env.client, task_queue=EXTERNAL_QUEUE, activities=external.activities()):
            handle = await _start(env.client, aeon_queue, graph, approvals={"default_ttl_seconds": 60})

            pending = None
            for _ in range(200):
                pending = await handle.query(GraphRunWorkflow.pending_approval)
                if pending:
                    break
                await asyncio.sleep(0.05)

            assert pending is not None, "the run never reported a pending approval"
            assert pending["node_id"] == "gated"
            # THE WORK HAS NOT HAPPENED. A gate that reported itself after the activity ran would
            # pass a test that only checked the final result.
            assert external.calls == [], (
                f"the external worker already ran {external.calls} while an approval was pending — "
                "the gate is decoration"
            )

            await handle.signal(
                GraphRunWorkflow.approve,
                {"approval_id": pending["approval_id"], "tool_call_hash": pending["tool_call_hash"]},
            )
            result = (await handle.result())["result"]

    assert len(external.calls) == 1, f"after approval the worker ran {external.calls}"
    assert _by_id(result, "gated")["result"]["processed"] == "needs-a-person"


@pytest.mark.asyncio
async def test_an_approval_for_other_parameters_fails_the_run_instead_of_being_ignored():
    """The parameter binding, and the behaviour is sharper than I assumed when writing this file.

    My first version signalled a wrong hash and expected it to be IGNORED, with the run carrying on
    to wait for a correct one. It is not ignored: `_await_approval` raises ApprovalDeniedError, so
    the run FAILS. That is the stronger contract and the right one — a decision that names different
    parameters is evidence that somebody approved something else, and treating it as noise would let
    a person's "yes" to one thing sit in the audit trail next to a different thing happening.

    The hash includes the TASK QUEUE for an activity (compute_activity_call_hash), which is what
    stops an approval for one queue from authorizing work on another machine.
    """
    external = ExternalWorker()
    graph = _graph_with(_activity_node("gated", "needs-a-person", requires_approval=True))

    async with await WorkflowEnvironment.start_local() as env:
        aeon_queue = f"aeon-{uuid.uuid4().hex[:8]}"
        async with Worker(
            env.client, task_queue=aeon_queue, workflows=[GraphRunWorkflow],
            activities=[execute_tool_activity, check_activity_policy_activity],
        ), Worker(env.client, task_queue=EXTERNAL_QUEUE, activities=external.activities()):
            handle = await _start(env.client, aeon_queue, graph, approvals={"default_ttl_seconds": 60})

            pending = None
            for _ in range(200):
                pending = await handle.query(GraphRunWorkflow.pending_approval)
                if pending:
                    break
                await asyncio.sleep(0.05)
            assert pending is not None

            await handle.signal(
                GraphRunWorkflow.approve,
                {"approval_id": pending["approval_id"], "tool_call_hash": "0" * 64},
            )
            with pytest.raises(WorkflowFailureError) as excinfo:
                await handle.result()

    assert "different tool_call_hash" in _why(excinfo.value), _why(excinfo.value)
    assert external.calls == [], (
        f"the worker ran {external.calls} on an approval that named other parameters"
    )


@pytest.mark.asyncio
async def test_the_activity_budget_stops_a_run_before_the_work_happens():
    """The counter is a cap, not a receipt — the same thing MDL-018's token ceiling had to prove."""
    external = ExternalWorker()
    graph = _graph_with(
        _activity_node("a1", "one"), _activity_node("a2", "two"), _activity_node("a3", "three")
    )

    async with await WorkflowEnvironment.start_local() as env:
        aeon_queue = f"aeon-{uuid.uuid4().hex[:8]}"
        async with Worker(
            env.client, task_queue=aeon_queue, workflows=[GraphRunWorkflow],
            activities=[execute_tool_activity, check_activity_policy_activity],
        ), Worker(env.client, task_queue=EXTERNAL_QUEUE, activities=external.activities()):
            handle = await _start(env.client, aeon_queue, graph, budgets={"max_activity_calls": 2})
            with pytest.raises(WorkflowFailureError) as excinfo:
                await handle.result()
            consumed = await handle.query(GraphRunWorkflow.budgets_consumed)

    assert "activity_calls_exceeded" in _why(excinfo.value), _why(excinfo.value)
    # TWO, not three: the node that tripped the limit never reached the worker. If this were three,
    # the budget would be reporting what was spent instead of capping it.
    assert len(external.calls) == 2, f"the external worker ran {len(external.calls)} activities, want 2"
    assert consumed["activity_calls"] == 2, consumed
