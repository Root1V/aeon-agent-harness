"""Activities: the only place non-determinism (and side effects) may live — see
docs/adr/0001-temporal-determinism-boundary.md. AgentRunWorkflow (workflows/agent_run.py) never
performs a write directly; it only calls these Activities and applies their typed result.
"""
from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Any

from temporalio import activity

from aeon_worker.idempotency import EffectsLedger, derive_idempotency_key

DEFAULT_LEDGER_PATH = os.environ.get("AEON_EFFECTS_LEDGER_PATH", "/tmp/aeon_effects_ledger.json")


@dataclass
class ExecuteToolInput:
    run_id: str
    node_id: str
    step_seq: int
    tool_name: str
    tool_args: dict[str, Any]
    # Test-only hook: when true, the FIRST (non-deduplicated) execution hard-kills the process
    # right after recording the effect but before returning to Temporal — simulating a worker
    # crash after the side effect landed but before the Activity completion was acknowledged.
    # See tests/integration/test_crash_resume.py. Never set in production code paths.
    simulate_crash_after_write: bool = False


@dataclass
class ExecuteToolOutput:
    deduplicated: bool
    idempotency_key: str
    result: dict[str, Any]


@activity.defn
async def execute_tool_activity(inp: ExecuteToolInput) -> ExecuteToolOutput:
    """Executes a tool call with idempotency (RUN-004). In production this delegates to the Tool
    Gateway (go/internal/store dedupe table, proto/aeon/v1/tool_gateway.proto ExecuteTool RPC);
    here it uses the local EffectsLedger so this Activity is self-contained for the integration
    test, which needs no Go services running to prove the crash/resume property end-to-end.
    """
    key = derive_idempotency_key(inp.run_id, inp.node_id, inp.step_seq, inp.tool_args)
    ledger = EffectsLedger(DEFAULT_LEDGER_PATH)
    deduplicated, result = ledger.record_once(
        key, {"tool_name": inp.tool_name, "tool_args": inp.tool_args, "status": "written"}
    )

    if not deduplicated and inp.simulate_crash_after_write:
        # The write above already landed on disk. We now die before Temporal receives
        # ActivityTaskCompleted, so the server will redeliver this task. The retry will find
        # `key` already in the ledger and take the `deduplicated=True` path above instead of
        # writing again — this is the whole point of the test.
        activity.logger.warning("simulate_crash_after_write=True: exiting process now")
        os._exit(1)  # noqa: SLF001 — deliberate hard kill, not a normal exception path.

    return ExecuteToolOutput(deduplicated=deduplicated, idempotency_key=key, result=result)
