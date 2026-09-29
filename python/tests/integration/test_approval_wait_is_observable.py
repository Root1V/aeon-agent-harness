"""test_approval_wait_is_observable — OBS-010's acceptance test.

WHAT WAS INVISIBLE. `INT-011` emits `argus.outcome=suspended` when Cedar answers `require_approval`,
and then the run blocks inside `GraphRunWorkflow._await_approval` (RUN-005) on a Temporal
`wait_condition`. A wait is not a call: no Activity runs, so no span exists, so a run parked on a
human for hours emitted exactly one record — taken before the wait began. We said as much to Argus on
2026-09-29 («el run todavía no se suspende de verdad por esa vía»). This is the test that closes it.

REAL THROUGHOUT: a real Temporal dev server, a real Worker running the real GraphRunWorkflow, a real
OTel Collector and a real Tempo queried over its HTTP API. Nothing is asserted against a span the test
itself constructed.

THE ASSERTION THAT MATTERS IS THE ONE TAKEN MID-WAIT. `test_a_wait_is_visible_while_it_is_still
_waiting` reads the record back from Tempo WHILE the workflow is still blocked and nobody has decided
anything. A test that approved first and then checked the spans would pass just as well against an
implementation that only records at the end — and that implementation is the one that answers nothing
during the hours it is the only thing happening.
"""
from __future__ import annotations

import asyncio
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

import pytest
from temporalio.client import Client, WorkflowFailureError
from temporalio.contrib.opentelemetry import TracingInterceptor
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from aeon_worker.activities.approval_activities import record_approval_wait_activity
from aeon_worker.activities.tool_activities import execute_tool_activity
from aeon_worker.graph import compute_tool_call_hash
from aeon_worker.workflows.graph_run import GraphRunWorkflow

TEMPO_URL = os.environ.get("AEON_TEST_TEMPO_QUERY_URL", "")
OTEL_ENDPOINT = os.environ.get("AEON_TEST_OTEL_ENDPOINT", "")

NODE_ID = "n0"
TOOL_NAME = "artifact.write"
TOOL_ARGS = {"path": "irreversible.txt"}
REAL_HASH = compute_tool_call_hash(NODE_ID, TOOL_NAME, TOOL_ARGS)


def _require_stack() -> None:
    missing = [n for n, v in (("AEON_TEST_TEMPO_QUERY_URL", TEMPO_URL), ("AEON_TEST_OTEL_ENDPOINT", OTEL_ENDPOINT)) if not v]
    if missing:
        pytest.skip(f"{', '.join(missing)} not set — needs the real obs stack (see make test-python-integration)")


def _graph(requires_approval: bool = True) -> dict:
    return {"id": NODE_ID, "kind": "tool_call", "tool_name": TOOL_NAME, "tool_args": TOOL_ARGS, "requires_approval": requires_approval}


def _init_tracing():
    # The SDK picks its protocol from the installed exporters and this image has both, so it must be
    # pinned or it speaks gRPC at an HTTP port and every export fails in a retry loop (found by running it).
    os.environ.setdefault("ARGUS_PROTOCOL", "http/protobuf")
    from aeon_observability import init_tracing

    return init_tracing("aeon-test-worker", "http://" + OTEL_ENDPOINT)


def _trace(trace_id: str) -> dict | None:
    try:
        with urllib.request.urlopen(f"{TEMPO_URL}/api/traces/{trace_id}", timeout=10) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            return None
        raise
    except urllib.error.URLError:
        return None


def _attr(value: dict) -> object:
    """One OTLP-JSON attribute value, whichever of its typed fields is populated."""
    for key in ("stringValue", "boolValue", "intValue", "doubleValue"):
        if key in value:
            return value[key]
    return None


def _spans(trace: dict) -> list[dict]:
    """Flatten a Tempo trace into dicts of {name, parent, status, attrs, service}."""
    out: list[dict] = []
    for batch in trace.get("batches", []):
        service = ""
        for a in batch.get("resource", {}).get("attributes", []):
            if a.get("key") == "service.name":
                service = a.get("value", {}).get("stringValue", "")
        for scope in batch.get("scopeSpans", []):
            for span in scope.get("spans", []):
                out.append({
                    "service": service,
                    "name": span.get("name", ""),
                    "parent": span.get("parentSpanId", ""),
                    "status": span.get("status", {}).get("code", 0),
                    "attrs": {a["key"]: _attr(a.get("value", {})) for a in span.get("attributes", [])},
                })
    return out


def _wait_records(trace: dict | None) -> list[dict]:
    return [s for s in _spans(trace or {}) if s["name"] == "approval.wait"]


def _poll_for_wait_records(trace_id: str, count: int, timeout: float = 90.0) -> list[dict]:
    """Poll Tempo until `count` approval.wait records are in the trace, or give up."""
    deadline = time.time() + timeout
    records: list[dict] = []
    while time.time() < deadline:
        records = _wait_records(_trace(trace_id))
        if len(records) >= count:
            return records
        time.sleep(2)
    return records


async def _pending(handle) -> dict:
    for _ in range(200):
        pending = await handle.query(GraphRunWorkflow.pending_approval)
        if pending is not None:
            return pending
        await asyncio.sleep(0.05)
    raise AssertionError("the run never reached the approval wait")


class _Harness:
    """A real Temporal, a real worker and a run started inside a real trace."""

    def __init__(self, env: WorkflowEnvironment, worker: Worker, handle, trace_id: str, run_id: str):
        self.env, self.worker, self.handle, self.trace_id, self.run_id = env, worker, handle, trace_id, run_id


async def _start_run(env: WorkflowEnvironment, worker: Worker, extra: dict | None = None):
    from aeon_observability import current_traceparent, run_span

    run_id = str(uuid.uuid4())
    request = {"run_id": run_id, "graph": _graph()}
    if extra:
        request.update(extra)
    # Started INSIDE a run span, so the workflow and its Activities hang off the same trace the caller
    # is in. That linkage is Temporal's TracingInterceptor doing its job, and it is half the feature:
    # a wait record in a trace of its own is a fact nobody following the run would ever reach.
    with run_span("invoke_agent", run_id=run_id):
        traceparent = current_traceparent()
        assert traceparent, "nothing is recording — this test would prove nothing"
        handle = await env.client.start_workflow(
            GraphRunWorkflow.run, request, id=f"graph-run-{run_id}", task_queue=worker.task_queue
        )
    return _Harness(env, worker, handle, traceparent.split("-")[1], run_id)


async def test_a_wait_is_visible_while_it_is_still_waiting():
    _require_stack()
    _init_tracing()
    async with await WorkflowEnvironment.start_local(interceptors=[TracingInterceptor()]) as env:
        async with Worker(
            # The interceptor goes on the CLIENT and the Worker inherits it — measured, because the
            # obvious reading is that a Worker needs its own and the redundant copy is invisible either
            # way. Removing it from the Worker alone changed nothing; removing it from the client made
            # this test fail with the record landing in a trace of its own.
            env.client, task_queue=f"obs010-{uuid.uuid4()}", workflows=[GraphRunWorkflow],
            activities=[execute_tool_activity, record_approval_wait_activity],
        ) as worker:
            h = await _start_run(env, worker)
            pending = await _pending(h.handle)

            # NOBODY HAS DECIDED ANYTHING YET. The workflow is blocked in wait_condition right now, and
            # this is the whole point of the feature: the record has to be readable during the wait.
            records = await asyncio.get_running_loop().run_in_executor(None, _poll_for_wait_records, h.trace_id, 1)
            assert records, (
                f"no approval.wait record reached Tempo while the run was still waiting (trace {h.trace_id}). "
                "The run is blocked on a human and nothing outside Temporal says so — the exact silence "
                "OBS-010 exists to remove"
            )
            assert len(records) == 1, f"expected exactly the opening record while still waiting, got {records}"
            rec = records[0]
            assert rec["attrs"].get("argus.outcome") == "suspended", (
                f"argus.outcome = {rec['attrs'].get('argus.outcome')!r} on a wait in progress, want 'suspended'. "
                "Argus's Step finaliser defaults this to 'ok', so a record that set nothing would store an "
                "unanswered approval as a completed step"
            )
            assert "aeon.approval.waited_ms" not in rec["attrs"], (
                "the opening record carries a duration. Nobody has waited yet, and a 0 here would put "
                "'waited no time' and 'has not finished waiting' in the same bucket"
            )
            assert rec["attrs"].get("aeon.approval.id") == pending["approval_id"]
            assert rec["attrs"].get("argus.run.id") == h.run_id

            # AND IT MUST NOT PAGE. A run waiting for a person is the system working; Argus routes a span
            # to its ~2s alert path on argus.hot, on a present argus.guardrail, or on an ERROR status, and
            # a long wait tripping any of the three is alert fatigue built from inside. They rejected our
            # `approval-required` guardrail on 2026-09-29 for precisely this.
            assert rec["attrs"].get("argus.hot") is not True, f"a wait in progress asks for a page: {rec['attrs']}"
            assert "argus.guardrail" not in rec["attrs"], f"a wait in progress asks for a page: {rec['attrs']}"
            assert rec["status"] in (0, 1), f"span status {rec['status']} — an ERROR status pages on its own"

            await h.handle.signal(GraphRunWorkflow.approve, {"approval_id": pending["approval_id"], "tool_call_hash": REAL_HASH})
            await h.handle.result()

            # And now the closing record, with the duration the wait really took.
            both = await asyncio.get_running_loop().run_in_executor(None, _poll_for_wait_records, h.trace_id, 2)
            assert len(both) == 2, f"the wait never got a closing record: {both}"
            closing = [r for r in both if r["attrs"].get("argus.outcome") != "suspended"]
            assert len(closing) == 1, f"records = {both}"
            assert closing[0]["attrs"].get("argus.outcome") == "ok", (
                f"a granted approval closed as {closing[0]['attrs'].get('argus.outcome')!r}; Argus's map "
                "(2026-09-29) is approval_granted -> ok"
            )
            assert int(closing[0]["attrs"].get("aeon.approval.waited_ms", -1)) >= 0, (
                f"no measured wait on the closing record: {closing[0]['attrs']}"
            )


async def test_the_record_closes_on_the_paths_that_raise():
    """A rejected and an expired wait, which are the two that leave through an exception.

    An opening record with no closing one reads as "still waiting" forever, so a denial that skipped
    the close would leave a permanently suspended approval in the store for a run that ended minutes
    earlier — an artefact asserting something the system is not doing, which is the failure this
    project keeps finding in its own artefacts.
    """
    _require_stack()
    _init_tracing()
    async with await WorkflowEnvironment.start_local(interceptors=[TracingInterceptor()]) as env:
        async with Worker(
            env.client, task_queue=f"obs010-{uuid.uuid4()}", workflows=[GraphRunWorkflow],
            activities=[execute_tool_activity, record_approval_wait_activity],
        ) as worker:
            rejected = await _start_run(env, worker)
            pending = await _pending(rejected.handle)
            await rejected.handle.signal(GraphRunWorkflow.reject, {"approval_id": pending["approval_id"], "tool_call_hash": REAL_HASH})
            with pytest.raises(WorkflowFailureError):
                await rejected.handle.result()

            expired = await _start_run(env, worker, {"approvals": {"default_ttl_seconds": 2}})
            await _pending(expired.handle)
            with pytest.raises(WorkflowFailureError):
                await expired.handle.result()

            loop = asyncio.get_running_loop()
            for harness, want in ((rejected, "denied"), (expired, "timeout")):
                records = await loop.run_in_executor(None, _poll_for_wait_records, harness.trace_id, 2)
                outcomes = sorted(r["attrs"].get("argus.outcome") for r in records)
                assert outcomes == sorted(["suspended", want]), (
                    f"outcomes for the {want} case = {outcomes} (trace {harness.trace_id}); Argus's map is "
                    "approval_denied -> denied and approval_expired -> timeout, and both must close"
                )
                closing = [r for r in records if r["attrs"].get("argus.outcome") == want][0]
                assert closing["attrs"].get("aeon.approval.waited_ms") is not None, (
                    f"the {want} record has no measured wait: {closing['attrs']}"
                )


async def test_a_run_started_before_obs_010_still_replays():
    """The history in tests/fixtures was recorded by the code as it was BEFORE this feature.

    MEASURED, NOT ASSUMED: replaying it against an unpatched version of this change fails with

        [TMPRL1100] Nondeterminism error: Activity type of scheduled event 'execute_tool_activity'
        does not match activity type of activity command 'record_approval_wait'

    Scheduling an Activity from workflow code adds a command to the history, and the runs that would
    have died are precisely the ones this feature is about: a run parked on a human decision is the
    longest-lived thing in the system, and it would have failed on its next workflow task — the
    approval signal that finally arrived. `workflow.patched` is what keeps them alive, and this
    fixture is the only thing that can prove it, because the code that produced it no longer exists.
    """
    from pathlib import Path

    from temporalio.client import WorkflowHistory
    from temporalio.worker import Replayer

    from aeon_worker.registry import WORKFLOWS

    fixture = Path(__file__).resolve().parents[1] / "fixtures" / "approval-history-pre-obs010.json"
    raw = fixture.read_text()
    # Asserted on the file, not on the replay: a fixture that already contained the new Activity would
    # replay clean and prove nothing, and it would look exactly like a pass.
    assert "record_approval_wait" not in raw and "obs-010-approval-wait-recorded" not in raw, (
        "the fixture already carries the new Activity or the patch marker — it is not a pre-OBS-010 history"
    )
    history = WorkflowHistory.from_json("graph-run-pre-obs010", raw)
    await Replayer(workflows=WORKFLOWS, interceptors=[TracingInterceptor()]).replay_workflow(history)
