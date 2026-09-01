"""INT-005's acceptance test: examples/openai-agents-interop runs a real `agents.Agent`/
`agents.Runner` loop inside a Temporal Activity, against a real (ephemeral) Temporal server and a
real separate worker OS process — the only thing faked is the Model Gateway itself (a tiny local
HTTP server standing in for aeon-modelgw's real POST /v1/chat/completions, INT-002). Unlike
test_crewai_interop_workflow.py/test_langgraph_interop_workflow.py's fake, which mimics the internal
/decide shape, this fake mimics the real OpenAI Chat Completions wire format, because the OpenAI
Agents SDK's OpenAIChatCompletionsModel speaks that format directly — no internal /decide call is
involved at all. The Agent's own native tool-calling loop genuinely decides to call the Aeon tool
(not an Activity-driven fixed sequence); its output flows back through the real Activity/workflow
boundary.
"""
from __future__ import annotations

import json
import os
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

from aeon_sdk.openai_agents_interop import start_openai_agents_interop_run
from tests.integration.test_deep_research_workflow import _assert_still_running

REPO_PYTHON_DIR = Path(__file__).resolve().parents[2]


class _FakeChatCompletionsHandler(BaseHTTPRequestHandler):
    """Stands in for aeon-modelgw's real POST /v1/chat/completions (INT-002). Dispatches purely on
    how many messages are in the conversation so far — the first turn (system + user) gets a
    tool-call response, and any later turn (after the SDK appends the assistant tool-call message and
    the tool's result) gets a final answer. This is the real OpenAI Chat Completions wire shape, not
    a simplified stand-in: the `openai` package parses this response with its own pydantic models."""

    def log_message(self, format: str, *args) -> None:  # noqa: A002
        pass

    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers["Content-Length"])
        body = json.loads(self.rfile.read(length))
        messages = body["messages"]

        if len(messages) <= 2:
            query = messages[-1]["content"]
            message = {
                "role": "assistant",
                "content": None,
                "tool_calls": [
                    {
                        "id": "call_1",
                        "type": "function",
                        "function": {"name": "search_web", "arguments": json.dumps({"query": query})},
                    }
                ],
            }
            finish_reason = "tool_calls"
        else:
            message = {"role": "assistant", "content": "Summary: the tool returned real search results."}
            finish_reason = "stop"

        payload = {
            "id": "chatcmpl-fake",
            "object": "chat.completion",
            "created": 1234567890,
            "model": body.get("model", "fake-profile"),
            "choices": [{"index": 0, "message": message, "finish_reason": finish_reason}],
            "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
        }
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(payload).encode())


def _start_fake_chat_completions_gateway() -> tuple[ThreadingHTTPServer, threading.Thread]:
    server = ThreadingHTTPServer(("127.0.0.1", 0), _FakeChatCompletionsHandler)
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


@pytest.mark.asyncio
async def test_openai_agents_interop_agent_runs_inside_a_real_activity():
    task_queue = f"aeon-openai-agents-interop-test-{uuid.uuid4().hex[:8]}"
    gateway_server, gateway_thread = _start_fake_chat_completions_gateway()
    try:
        modelgw_addr = f"127.0.0.1:{gateway_server.server_address[1]}"

        async with await WorkflowEnvironment.start_local() as env:
            address = env.client.service_client.config.target_host
            worker = _spawn_worker(address, task_queue, modelgw_addr)
            try:
                time.sleep(0.5)  # give an early connection failure a moment to surface
                _assert_still_running(worker)
                client: Client = env.client

                result = await start_openai_agents_interop_run(
                    "what is the state of the art in agent harnesses?",
                    model="fake-profile",
                    temporal_address=address,
                    task_queue=task_queue,
                )

                assert result.final_output, "the agent's own Runner.run never produced a final output"
                # "written" is the real execute_tool's (RUN-004's idempotent ledger) status field —
                # not a fake double's — proving the Agent's own native tool-calling loop genuinely
                # triggered a real tool call, not a fixed Activity-driven sequence.
                assert result.tool_result.get("status") == "written", f"the agent's native tool call never ran: {result.tool_result}"
                assert result.tool_result.get("tool_name") == "search.web"

                handle = client.get_workflow_handle(result.run_id)
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
