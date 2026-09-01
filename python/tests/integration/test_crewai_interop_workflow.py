"""INT-004's acceptance test: examples/crewai-interop runs a real crewai.Crew inside a Temporal
Activity, against a real (ephemeral) Temporal server and a real separate worker OS process — the
only thing faked is the Model Gateway itself (a tiny local HTTP server standing in for a real
provider, same as test_deep_research_workflow.py). The crew's Agent genuinely calls AeonLLM, and
the "research" step's Aeon tool call genuinely executes; their output flows back through the real
Activity/workflow boundary.
"""
from __future__ import annotations

import subprocess
import time
import uuid

import pytest
from temporalio.client import Client
from temporalio.testing import WorkflowEnvironment

from aeon_sdk.crewai_interop import start_crewai_interop_run
from tests.integration.test_deep_research_workflow import _assert_still_running, _spawn_worker, _start_fake_model_gateway


@pytest.mark.asyncio
async def test_crewai_interop_crew_runs_inside_a_real_activity():
    task_queue = f"aeon-crewai-interop-test-{uuid.uuid4().hex[:8]}"
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

                result = await start_crewai_interop_run(
                    "what is the state of the art in agent harnesses?",
                    candidates=[{"provider": "fake", "model": "fake-model", "priority": 0}],
                    model="fake-model",
                    temporal_address=address,
                    task_queue=task_queue,
                )

                assert result.plan, "the crew's AeonLLM call never produced output"
                # "written" is the real execute_tool's (RUN-004's idempotent ledger) status field —
                # not a fake double's — proving the research step's tool call genuinely ran.
                assert result.tool_result.get("status") == "written", f"the research step's tool call never ran: {result.tool_result}"
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
