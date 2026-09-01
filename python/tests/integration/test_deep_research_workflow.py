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


def _spawn_worker(address: str, task_queue: str, modelgw_addr: str) -> subprocess.Popen:
    env = os.environ.copy()
    env["AEON_TEMPORAL_ADDRESS"] = address
    env["AEON_TASK_QUEUE"] = task_queue
    env["AEON_MODELGW_ADDR"] = modelgw_addr
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
