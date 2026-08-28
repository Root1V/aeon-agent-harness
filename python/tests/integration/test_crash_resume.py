"""test_crash_resume_no_duplicate_write — the acceptance test named in roadmap.md RUN-004 and in
docs/adr/0001-temporal-determinism-boundary.md.

Spec §9 acceptance criterion: "a run stopped by a process crash can resume from checkpoint and
does not repeat an already-confirmed write."

This test proves it end-to-end against a REAL Temporal server (an ephemeral local instance via
temporalio's test environment) and a REAL separate worker OS process:

  1. Start an ephemeral local Temporal server.
  2. Spawn worker subprocess A (`python -m aeon_worker`) on a dedicated task queue.
  3. Start AgentRunWorkflow with simulate_crash_after_write=True.
  4. The Activity writes to the on-disk EffectsLedger, then calls os._exit(1) BEFORE Temporal
     records ActivityTaskCompleted — this kills subprocess A, genuinely, at the OS level.
  5. Wait for subprocess A to die; spawn worker subprocess B (a fresh process) on the same task
     queue. Temporal redelivers the still-outstanding Activity task to it.
  6. The Activity runs again in subprocess B, finds the idempotency_key already recorded, and
     returns the deduplicated result WITHOUT writing again (simulate_crash_after_write is not
     retriggered on a deduplicated path — see tool_activities.py).
  7. The workflow completes. Assert: exactly one write in the ledger for this key, and the
     workflow's own result reports deduplicated=True (proving it observed the resumed path).

Requires network access on first run (temporalio downloads a local Temporal CLI binary once,
cached under ~/.cache thereafter) and the `temporalio` package installed (see pyproject.toml).
"""
from __future__ import annotations

import asyncio
import json
import os
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path

import pytest
from temporalio.client import Client
from temporalio.testing import WorkflowEnvironment

from aeon_worker.idempotency import EffectsLedger, derive_idempotency_key
from aeon_worker.workflows.agent_run import AgentRunWorkflow

REPO_PYTHON_DIR = Path(__file__).resolve().parents[2]


def _spawn_worker(address: str, task_queue: str, ledger_path: str, env_extra: dict[str, str] | None = None) -> subprocess.Popen:
    env = os.environ.copy()
    env["AEON_TEMPORAL_ADDRESS"] = address
    env["AEON_TASK_QUEUE"] = task_queue
    env["AEON_EFFECTS_LEDGER_PATH"] = ledger_path
    env["PYTHONPATH"] = str(REPO_PYTHON_DIR) + os.pathsep + env.get("PYTHONPATH", "")
    if env_extra:
        env.update(env_extra)
    return subprocess.Popen(
        [sys.executable, "-m", "aeon_worker"],
        cwd=str(REPO_PYTHON_DIR),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )


def _wait_for_exit(proc: subprocess.Popen, timeout_s: float = 20.0) -> int:
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        rc = proc.poll()
        if rc is not None:
            return rc
        time.sleep(0.2)
    raise TimeoutError("worker subprocess did not exit (crash simulation did not trigger)")


@pytest.mark.asyncio
async def test_crash_resume_no_duplicate_write():
    task_queue = f"aeon-crash-test-{uuid.uuid4().hex[:8]}"
    run_id = str(uuid.uuid4())
    node_id = "node-0"
    step_seq = 0
    tool_args = {"path": "reports/final.md", "content": "hello from the crash-resume test"}
    expected_key = derive_idempotency_key(run_id, node_id, step_seq, tool_args)

    with tempfile.TemporaryDirectory() as tmpdir:
        ledger_path = str(Path(tmpdir) / "effects_ledger.json")

        async with await WorkflowEnvironment.start_local() as env:
            address = env.client.service_client.config.target_host

            worker_a = _spawn_worker(address, task_queue, ledger_path)
            try:
                client: Client = env.client
                handle = await client.start_workflow(
                    AgentRunWorkflow.run,
                    {
                        "run_id": run_id,
                        "node_id": node_id,
                        "step_seq": step_seq,
                        "tool_name": "artifact.write",
                        "tool_args": tool_args,
                        "simulate_crash_after_write": True,
                    },
                    id=f"agent-run-{run_id}",
                    task_queue=task_queue,
                )

                # Worker A crashes itself (os._exit(1)) right after recording the write.
                rc_a = _wait_for_exit(worker_a, timeout_s=30.0)
                assert rc_a != 0, "worker A was expected to hard-exit via os._exit(1)"

                # The write must have landed exactly once before the crash.
                ledger = EffectsLedger(ledger_path)
                assert ledger.count(expected_key) == 1, "expected exactly one write recorded before the simulated crash"

                # Resume: a genuinely fresh worker process picks up the still-outstanding Activity task.
                worker_b = _spawn_worker(address, task_queue, ledger_path)
                try:
                    result = await handle.result()
                finally:
                    worker_b.terminate()
                    try:
                        worker_b.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        worker_b.kill()

                assert ledger.count(expected_key) == 1, "resume must not duplicate the write"
                assert result["deduplicated"] is True, "the resumed attempt should observe the dedup path"
                assert result["idempotency_key"] == expected_key
                assert result["result"]["status"] == "written"
            finally:
                if worker_a.poll() is None:
                    worker_a.terminate()
