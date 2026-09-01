"""INT-007's acceptance test: examples/claude-agent-sdk-interop runs a real
`claude_agent_sdk.ClaudeSDKClient` turn loop (backed by the real `claude` Claude Code CLI,
spawned as a subprocess) inside a Temporal Activity, against a real (ephemeral) Temporal server and
a real separate worker OS process.

Unlike every other interop test in this project, the thing faked here is not aeon-modelgw (there is
no Aeon-side endpoint in this integration's model path at all — see
aeon_adapters.claude_agent_sdk.adapter's module docstring) but Anthropic's own Messages API
(`POST /v1/messages`), which the real `claude` CLI is redirected to via `ANTHROPIC_BASE_URL` set on
the spawned worker process's environment (the CLI subprocess inherits it, verified against the SDK's
own transport source). This keeps the test hermetic (no live LLM, no cost, no real credentials ever
touched) while still exercising the real CLI subprocess and the real Claude Agent SDK end to end.

The tool call is driven entirely by Claude's own native reasoning loop against a real in-process MCP
tool bound to Aeon's Tool Gateway (`execute_tool`, RUN-004) — every built-in Claude Code tool
(Bash, Read, Write, ...) is excluded, so this is the only tool Claude could possibly have called.
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

from aeon_sdk.claude_agent_interop import start_claude_agent_interop_run
from tests.integration.test_deep_research_workflow import _assert_still_running

REPO_PYTHON_DIR = Path(__file__).resolve().parents[2]


class _FakeAnthropicMessagesHandler(BaseHTTPRequestHandler):
    """Stands in for Anthropic's real POST /v1/messages. Dispatches on whether the conversation so
    far already contains a tool_result content block: the first turn gets a tool_use response, and
    any later turn gets a final text answer — same dispatch rule every other interop test's fake
    uses, adapted to the Messages API's own shape (content blocks, not OpenAI-style tool_calls)."""

    def log_message(self, format: str, *args) -> None:  # noqa: A002
        pass

    def do_HEAD(self) -> None:  # noqa: N802
        self.send_response(200)
        self.end_headers()

    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length)) if length else {}
        messages = body.get("messages", [])

        has_tool_result = any(
            isinstance(m.get("content"), list) and any(c.get("type") == "tool_result" for c in m["content"]) for m in messages
        )

        if not has_tool_result:
            content = [{"type": "tool_use", "id": "toolu_1", "name": "mcp__aeon__search_web", "input": {"query": "agent harnesses"}}]
            stop_reason = "tool_use"
        else:
            content = [{"type": "text", "text": "Summary: the tool returned real search results."}]
            stop_reason = "end_turn"

        payload = {
            "id": "msg_fake",
            "type": "message",
            "role": "assistant",
            "content": content,
            "model": body.get("model", "claude-fake"),
            "stop_reason": stop_reason,
            "usage": {"input_tokens": 10, "output_tokens": 5},
        }
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(payload).encode())


def _start_fake_anthropic_gateway() -> tuple[ThreadingHTTPServer, threading.Thread]:
    server = ThreadingHTTPServer(("127.0.0.1", 0), _FakeAnthropicMessagesHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, thread


def _spawn_worker(address: str, task_queue: str, anthropic_base_url: str) -> subprocess.Popen:
    env = os.environ.copy()
    env["AEON_TEMPORAL_ADDRESS"] = address
    env["AEON_TASK_QUEUE"] = task_queue
    # Overriding both, unconditionally, ensures the real `claude` CLI subprocess this worker spawns
    # can never reach real Anthropic infrastructure or use a real credential, regardless of what the
    # host environment happens to have set.
    env["ANTHROPIC_BASE_URL"] = anthropic_base_url
    env["ANTHROPIC_API_KEY"] = "sk-fake-unused"
    env["DISABLE_TELEMETRY"] = "1"
    env["DISABLE_AUTOUPDATER"] = "1"
    env["DISABLE_ERROR_REPORTING"] = "1"
    env["PYTHONPATH"] = str(REPO_PYTHON_DIR) + os.pathsep + env.get("PYTHONPATH", "")
    return subprocess.Popen(
        [sys.executable, "-m", "aeon_worker"], cwd=str(REPO_PYTHON_DIR), env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT
    )


@pytest.mark.asyncio
async def test_claude_agent_interop_agent_runs_inside_a_real_activity():
    task_queue = f"aeon-claude-agent-interop-test-{uuid.uuid4().hex[:8]}"
    gateway_server, gateway_thread = _start_fake_anthropic_gateway()
    try:
        anthropic_base_url = f"http://127.0.0.1:{gateway_server.server_address[1]}"

        async with await WorkflowEnvironment.start_local() as env:
            address = env.client.service_client.config.target_host
            worker = _spawn_worker(address, task_queue, anthropic_base_url)
            try:
                time.sleep(0.5)  # give an early connection failure a moment to surface
                _assert_still_running(worker)
                client: Client = env.client

                result = await start_claude_agent_interop_run(
                    "what is the state of the art in agent harnesses?",
                    temporal_address=address,
                    task_queue=task_queue,
                )

                assert result.final_output, "Claude's own turn loop never produced a final output"
                # "written" is the real execute_tool's (RUN-004's idempotent ledger) status field —
                # not a fake double's — proving Claude's own native tool-calling loop genuinely
                # triggered a real, Aeon-governed tool call.
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
