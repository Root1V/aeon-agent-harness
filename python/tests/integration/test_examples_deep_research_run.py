"""Verifies examples/deep-research/run.py itself — the manifest-loading and profile-resolution
glue, not just aeon_sdk.deep_research.start_deep_research_run directly (already proven by
test_deep_research_workflow.py). This is what a user running `aeon run examples/deep-research
"query"` (DX-002) actually experiences: the real checked-in agent.yaml and
model_policy_bundle.yaml, loaded and resolved for real, driving a real workflow execution.
"""
from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

import pytest
from temporalio.testing import WorkflowEnvironment

from aeon_sdk.deep_research import DEFAULT_TASK_QUEUE
from tests.integration.test_deep_research_workflow import _spawn_worker, _start_fake_model_gateway

REPO_ROOT = Path(__file__).resolve().parents[3]
RUN_SCRIPT = REPO_ROOT / "examples" / "deep-research" / "run.py"
PYTHON_DIR = REPO_ROOT / "python"


@pytest.mark.asyncio
async def test_examples_deep_research_run_script_produces_a_report():
    gateway_server, gateway_thread = _start_fake_model_gateway()
    try:
        modelgw_addr = f"127.0.0.1:{gateway_server.server_address[1]}"

        async with await WorkflowEnvironment.start_local() as env:
            address = env.client.service_client.config.target_host
            # run.py doesn't expose task_queue, so the spawned worker must use the SDK's own
            # default queue — the same one start_deep_research_run starts workflows on.
            worker = _spawn_worker(address, DEFAULT_TASK_QUEUE, modelgw_addr)
            try:
                env_vars = os.environ.copy()
                env_vars["AEON_TEMPORAL_ADDRESS"] = address
                env_vars["PYTHONPATH"] = str(PYTHON_DIR) + os.pathsep + env_vars.get("PYTHONPATH", "")

                result = subprocess.run(
                    [sys.executable, str(RUN_SCRIPT), "what is the state of the art in agent harnesses?"],
                    cwd=str(PYTHON_DIR),
                    env=env_vars,
                    capture_output=True,
                    text=True,
                    timeout=60,
                )
                assert result.returncode == 0, f"run.py exited {result.returncode}:\nstdout: {result.stdout}\nstderr: {result.stderr}"
                assert "sufficient: True" in result.stdout
                assert "Synthesized report" in result.stdout
                assert "run_id: deep-research-" in result.stdout
            finally:
                worker.terminate()
                try:
                    worker.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    worker.kill()
    finally:
        gateway_server.shutdown()
        gateway_thread.join(timeout=5)
