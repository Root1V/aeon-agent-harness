"""DX-001's acceptance test: DeepResearchWorkflow — Planner -> Researchers -> Sufficiency Gate ->
Reporter -> Citation Verifier — runs for real against a real (ephemeral) Temporal server and a real
separate worker OS process, with every model call going out over real HTTP to a Model Gateway. The
only thing faked is the Model Gateway itself (a tiny local HTTP server standing in for a real
provider) — no live LLM, no cost, but a genuinely real Temporal/Activity/HTTP round trip end to end.
This is what "examples/deep-research corre con aeon_sdk" (roadmap.md DX-001) means concretely.
"""
from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest
from temporalio.client import Client
from temporalio.testing import WorkflowEnvironment

from aeon_sdk.deep_research import start_deep_research_run

REPO_PYTHON_DIR = Path(__file__).resolve().parents[2]


def _normalized(content: dict | str) -> dict:
    text = content if isinstance(content, str) else json.dumps(content)
    return {
        "model": "fake-model",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
    }


# OBS-003b: every /decide body this fake receives, so a test can ask who each model call said it was
# for. Recorded here and not asserted here — the fake's job is to answer, not to judge.
DECIDE_REQUESTS: list[dict] = []


class _FakeModelGatewayHandler(BaseHTTPRequestHandler):
    """Stands in for a real Model Gateway /decide endpoint. Which shape to return is decided by
    reading the ACTUAL system prompt in the request — the same distinguishing text
    aeon_profiles.deep_research.{planner,researcher,reporter} each generate — not a hardcoded call
    sequence, so this fake reacts correctly regardless of how many subtasks the real Planner asks
    for or how many concurrent Researchers are calling in at once.
    """

    def log_message(self, format: str, *args) -> None:  # noqa: A002 — matches BaseHTTPRequestHandler's signature
        pass

    def do_POST(self) -> None:  # noqa: N802 — required name by http.server
        length = int(self.headers["Content-Length"])
        body = json.loads(self.rfile.read(length))
        DECIDE_REQUESTS.append(body)
        rendered_context = body["rendered_context"]
        messages = rendered_context["messages"]
        system_prompt = messages[0]["content"]

        if "Research Planner" in system_prompt:
            content = self._plan_response(messages[-1]["content"])
        elif "isolated Researcher" in system_prompt:
            content = self._researcher_response(messages)
        elif "Reporter" in system_prompt:
            content = self._reporter_response(system_prompt)
        elif "research planner for a LangGraph interop example" in system_prompt:
            # examples/langgraph-interop (INT-001): this node reads the model's content as plain
            # prose, not JSON — unlike DR-001's Planner, it never parses/validates it.
            content = f"Plan: investigate {messages[-1]['content']!r} via a single web search."
        elif "CrewAI interop example" in system_prompt:
            # examples/crewai-interop (INT-004): CrewAI builds its own internal prompt from the
            # Agent's role/goal/backstory and the Task's description — not a shape this fake
            # controls or needs to parse. Read as plain prose (CrewOutput.raw), same as the
            # LangGraph interop branch above.
            content = "Plan: investigate the query via a single web search."
        elif "Reflection step for an agent harness" in system_prompt:
            # MEM-003. The candidate's evidence_refs are taken FROM THE PROMPT's own evidence list, not
            # invented: the Reflector rejects a candidate citing anything the run did not produce, so a
            # fake that made one up would exercise the rejection path instead of the happy one — and the
            # rejection path has its own test.
            available = re.findall(r"^- (\S+)$", system_prompt, re.MULTILINE)
            content = {
                "candidates": [
                    {
                        "type": "PROCEDURAL",
                        "scope": "project",
                        "content": "Searching per-subtask before synthesising produced a verified report.",
                        "evidence_refs": available[:1],
                        "confidence": 0.7,
                    }
                ]
            }
        else:
            content = {"error": f"fake gateway does not recognize this system prompt: {system_prompt!r}"}

        payload = json.dumps(
            {"provider_used": "fake", "model": rendered_context.get("model", "fake-model"), "output": _normalized(content)}
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(payload)

    @staticmethod
    def _plan_response(query: str) -> dict:
        subtasks = [
            {
                "id": f"st-{i}",
                "description": f"Investigate facet {i} of: {query}",
                "coverage_topic": f"facet-{i}",
                "budget": {"max_tool_calls": 2, "max_model_calls": 2},
            }
            for i in range(3)
        ]
        return {"query": query, "subtasks": subtasks}

    @staticmethod
    def _researcher_response(messages: list[dict]) -> dict:
        if len(messages) <= 2:
            topic = messages[1]["content"]
            return {"action": "CALL_TOOL", "tool_name": "search.web", "args": {"query": topic}}
        return {"action": "FINISH", "message": f"summary for: {messages[1]['content']}"}

    @staticmethod
    def _reporter_response(system_prompt: str) -> dict:
        # build_reporter_request's instructions themselves mention the literal token "[claim_id]"
        # as a format example — only the "Allowed claims:" section (each line "[<real-id>] ...")
        # should be scanned, or that literal example text gets misread as a real claim id.
        _, _, claims_section = system_prompt.partition("Allowed claims:\n")
        claim_ids = re.findall(r"\[([\w.-]+)\]", claims_section)
        return {"text": "Synthesized report covering every allowed claim.", "cited_claim_ids": claim_ids}


def _start_fake_model_gateway() -> tuple[ThreadingHTTPServer, threading.Thread]:
    server = ThreadingHTTPServer(("127.0.0.1", 0), _FakeModelGatewayHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, thread


def _spawn_worker(address: str, task_queue: str, modelgw_addr: str, controlplane_addr: str = "") -> subprocess.Popen:
    env = os.environ.copy()
    env["AEON_TEMPORAL_ADDRESS"] = address
    env["AEON_TASK_QUEUE"] = task_queue
    env["AEON_MODELGW_ADDR"] = modelgw_addr
    # MEM-003: where write_memory_candidates_activity posts. Left unset for the runs that do not
    # reflect, so the activity's "no memory store configured" branch is the one that reports it rather
    # than the test pretending a store exists.
    if controlplane_addr:
        env["AEON_CONTROLPLANE_ADDR"] = controlplane_addr
    env["PYTHONPATH"] = str(REPO_PYTHON_DIR) + os.pathsep + env.get("PYTHONPATH", "")
    return subprocess.Popen(
        [sys.executable, "-m", "aeon_worker"], cwd=str(REPO_PYTHON_DIR), env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT
    )


def _assert_still_running(proc: subprocess.Popen) -> None:
    """A worker that already exited (e.g. failed to connect to Temporal) never becomes ready —
    fail fast with its output rather than waiting for the workflow to time out. Not starting the
    workflow until the worker is fully polling isn't necessary: Temporal queues the workflow task
    until a worker picks it up, exactly like tests/integration/test_crash_resume.py relies on."""
    if proc.poll() is not None:
        output = proc.stdout.read().decode(errors="replace") if proc.stdout else ""
        raise RuntimeError(f"worker exited early (code {proc.returncode}):\n{output}")


@pytest.mark.asyncio
async def test_deep_research_workflow_produces_a_verified_report_end_to_end():
    task_queue = f"aeon-deep-research-test-{uuid.uuid4().hex[:8]}"
    gateway_server, gateway_thread = _start_fake_model_gateway()
    try:
        modelgw_addr = f"127.0.0.1:{gateway_server.server_address[1]}"

        async with await WorkflowEnvironment.start_local() as env:
            address = env.client.service_client.config.target_host
            worker = _spawn_worker(address, task_queue, modelgw_addr)
            try:
                time.sleep(0.5)  # give an early connection failure a moment to surface
                _assert_still_running(worker)
                client: Client = env.client

                # start_deep_research_run itself connects its own Client — point it at the same
                # ephemeral server via the address the SDK call accepts.
                report = await start_deep_research_run(
                    "what is the state of the art in agent harnesses?",
                    candidates=[{"provider": "fake", "model": "fake-model", "priority": 0}],
                    model="fake-model",
                    temporal_address=address,
                    task_queue=task_queue,
                )

                assert report.sufficient is True, f"expected the run to be sufficient, got topics_to_replan={report.topics_to_replan}"
                assert report.topics_to_replan == []
                assert len(report.cited_claim_ids) == 3, "one claim per subtask, all cited"
                assert "Synthesized report" in report.report_text

                # A real, durable workflow execution really did happen — not just the SDK call
                # object — confirmed via the same Temporal client used to spawn the worker.
                handle = client.get_workflow_handle(report.run_id)
                describe = await handle.describe()
                assert describe.status.name == "COMPLETED"
            finally:
                worker.terminate()
                try:
                    worker.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    worker.kill()
    finally:
        gateway_server.shutdown()
        gateway_thread.join(timeout=5)


CONTROLPLANE_ADDR = os.environ.get("AEON_TEST_CONTROLPLANE_ADDR", "")


@pytest.mark.asyncio
async def test_every_model_call_in_a_run_says_which_run_and_agent_it_is_for():
    """OBS-003b: the cost of a run is attributable because every call that spends money names itself.

    WHAT THIS CATCHES, and it is the defect it was written for. `decideRequest` has accepted
    `run_id`/`agent_manifest_ref` since OBS-003 and the ledger has had nullable columns for both since
    then — and no Python caller filled either, so `GET /finops/costs` could aggregate per model and the
    other half of OBS-003's own title had nothing to render. Both ends built, the wire between them
    never run, and nothing failing.

    ASSERTED PER STAGE, not in aggregate. A Deep Research run makes model calls from three different
    places (Planner, Researchers, Reporter) and each one threads the identity separately, so "some
    call carried it" is the assertion that passes while two of the three are still anonymous — which
    is exactly the state before this change, where only the Researcher had the fields at all (MDL-015
    added `agent_manifest_ref` there as a policy principal, for an unrelated reason).

    Real throughout except the provider: a real Temporal server, a real worker process, the real
    workflow and the real activities. The Model Gateway is the local double this module already uses,
    and it is the right place to look from — it is where the request arrives.
    """
    task_queue = f"aeon-cost-attrib-{uuid.uuid4().hex[:8]}"
    agent_ref = "deep-research-general@0.1.0"
    DECIDE_REQUESTS.clear()
    gateway_server, gateway_thread = _start_fake_model_gateway()
    try:
        modelgw_addr = f"127.0.0.1:{gateway_server.server_address[1]}"
        async with await WorkflowEnvironment.start_local() as env:
            address = env.client.service_client.config.target_host
            worker = _spawn_worker(address, task_queue, modelgw_addr)
            try:
                time.sleep(0.5)
                _assert_still_running(worker)
                report = await start_deep_research_run(
                    "what is the state of the art in agent harnesses?",
                    candidates=[{"provider": "fake", "model": "fake-model", "priority": 0}],
                    model="fake-model",
                    temporal_address=address,
                    task_queue=task_queue,
                    agent_manifest_ref=agent_ref,
                )
            finally:
                worker.terminate()
                try:
                    worker.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    worker.kill()
    finally:
        gateway_server.shutdown()
        gateway_thread.join(timeout=5)

    assert DECIDE_REQUESTS, "the run made no model calls at all — nothing here would mean anything"

    # Every stage, identified the way the gateway double already identifies them: by the system prompt
    # each one really sends. Keyed on the same strings the double branches on, so a stage that stops
    # being recognised here is a stage the double stopped answering too.
    stages = {
        "Planner": "Research Planner",
        "Researcher": "isolated Researcher",
        "Reporter": "Reporter",
    }
    by_stage: dict[str, list[dict]] = {name: [] for name in stages}
    unclassified: list[str] = []
    for body in DECIDE_REQUESTS:
        prompt = body["rendered_context"]["messages"][0]["content"]
        for name, marker in stages.items():
            if marker in prompt:
                by_stage[name].append(body)
                break
        else:
            unclassified.append(prompt[:120])

    assert not unclassified, (
        f"a model call came from a stage this test does not know about: {unclassified}. It may be "
        "spending money anonymously, which is the whole point of this assertion"
    )
    for name, bodies in by_stage.items():
        assert bodies, f"the {name} stage made no model call — this test cannot say whether it attributes its cost"
        for body in bodies:
            assert body.get("run_id") == report.run_id, (
                f"a {name} model call sent run_id={body.get('run_id')!r}, want {report.run_id!r}. Its cost "
                "lands in the ledger with run_id NULL, which is indistinguishable from a call made "
                "outside any run — so this run's total is a lower bound and nothing says so"
            )
            assert body.get("agent_manifest_ref") == agent_ref, (
                f"a {name} model call sent agent_manifest_ref={body.get('agent_manifest_ref')!r}, want {agent_ref!r}"
            )

    print(
        "\nOBS-003b: "
        + ", ".join(f"{name}={len(bodies)}" for name, bodies in by_stage.items())
        + f" model call(s), all attributed to run {report.run_id}"
    )


@pytest.mark.asyncio
async def test_reflection_writes_real_candidates_from_a_real_run():
    """MEM-003's acceptance test: a finished run proposes memory candidates and they are PERSISTED.

    What this closes, in the backlog's own words: `aeon_memory/reflection.py` was a pure module tested
    with a fake `decide`, and nothing called it from a real run. The logic was real and the integration
    was not — the same state DR-001..004 were in before DX-001.

    Real throughout: a real Temporal server, a real worker OS process, a real HTTP round trip to the
    model gateway for the reflection call, and a REAL control plane over Postgres receiving the write.
    The one thing faked is the model itself.

    THE ASSERTION THAT MATTERS IS NOT THE COUNT. It is that what lands in the store is a CANDIDATE:
    MemoryStore.WriteCandidate forces status=CANDIDATE regardless of what the caller sends, so a model
    that proposed an already-ACTIVE memory still writes into the quarantine pipeline. Checking the count
    alone would pass against a store that accepted anything.
    """
    if not CONTROLPLANE_ADDR:
        pytest.skip("AEON_TEST_CONTROLPLANE_ADDR not set — needs a real control plane over Postgres")

    task_queue = f"aeon-reflection-test-{uuid.uuid4().hex[:8]}"
    DECIDE_REQUESTS.clear()  # OBS-003b, asserted at the end
    gateway_server, _ = _start_fake_model_gateway()
    try:
        modelgw_addr = f"127.0.0.1:{gateway_server.server_address[1]}"
        async with await WorkflowEnvironment.start_local() as env:
            address = env.client.service_client.config.target_host
            worker = _spawn_worker(address, task_queue, modelgw_addr, CONTROLPLANE_ADDR)
            try:
                time.sleep(0.5)
                _assert_still_running(worker)

                report = await start_deep_research_run(
                    "what is the state of the art in agent harnesses?",
                    candidates=[{"provider": "fake", "model": "fake-model", "priority": 0}],
                    model="fake-model",
                    temporal_address=address,
                    task_queue=task_queue,
                    reflect=True,
                )
            finally:
                # terminate then KILL, because a graceful stop is not guaranteed to be quick any more:
                # the Argus SDK flushes and RETRIES its exporters on shutdown, and in this test there is
                # no collector to accept them. A test that fails because a subprocess took eleven
                # seconds to die reports nothing about the feature it is named after.
                worker.terminate()
                try:
                    worker.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    worker.kill()
                    worker.wait(timeout=10)

        # OBS-003b, asserted here because this is the only test with a real control plane and therefore
        # the only one where the reflection call actually happens. Reflection is an EXTRA model call per
        # run — the `reflect` flag's own note says a caller who has not thought about memory should not
        # silently start paying for one — so it is the call most worth being able to attribute, and the
        # one whose cost a caller is least likely to be expecting.
        reflection_calls = [
            b for b in DECIDE_REQUESTS
            if "Reflection step for an agent harness" in b["rendered_context"]["messages"][0]["content"]
        ]
        assert reflection_calls, "no reflection model call was made, so nothing here is about reflection"
        for body in reflection_calls:
            assert body.get("run_id") == report.run_id, (
                f"the reflection call sent run_id={body.get('run_id')!r}, want {report.run_id!r} — the one "
                "model call a caller did not ask for is the one that must not be anonymous in the ledger"
            )

        assert report.reflection_note == "", f"reflection did not run: {report.reflection_note}"
        assert report.memory_candidates_written == 1, (
            f"wrote {report.memory_candidates_written} candidates, want 1 — and a note of "
            f"{report.reflection_note!r}"
        )

        # Read back BY THE DETERMINISTIC ID, through the governed surface rather than the database.
        #
        # Computing the id here rather than searching for the record is what makes this test also check
        # the idempotency property: if the activity ever went back to a random uuid, this lookup 404s.
        # And it is the only thing that makes a RETRY safe — MemoryStore.Create has no ON CONFLICT, so a
        # random id would let a retried write create a second, indistinguishable candidate.
        import urllib.request

        from aeon_worker.activities.memory_activities import ReflectedCandidate, candidate_memory_id

        expected_id = candidate_memory_id(
            report.run_id,
            ReflectedCandidate(
                type="PROCEDURAL",
                scope="project",
                content="Searching per-subtask before synthesising produced a verified report.",
            ),
        )
        with urllib.request.urlopen(f"http://{CONTROLPLANE_ADDR}/memory/{expected_id}?tenant_id=default", timeout=20) as resp:
            record = json.loads(resp.read())

        assert record["status"] == "CANDIDATE", (
            f"status = {record['status']!r}. WriteCandidate is supposed to force CANDIDATE regardless of "
            "what the caller sends — anything else means a model's proposal reached active memory"
        )
        assert record["type"] == "PROCEDURAL"
        assert report.run_id in record["source_run_ids"], "the candidate is not attributed to this run"
        # Grounded in a claim the run actually produced AND the verifier actually accepted.
        assert record["evidence_refs"], "the candidate cites no evidence, so nothing ties it to this run"
        assert set(record["evidence_refs"]) <= set(report.cited_claim_ids), (
            f"candidate cites {record['evidence_refs']} and the report cited {report.cited_claim_ids} — a "
            "memory grounded in a claim the Citation Verifier did not accept"
        )
    finally:
        gateway_server.shutdown()
