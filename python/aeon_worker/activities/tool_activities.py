"""Activities: the only place non-determinism (and side effects) may live — see
docs/adr/0001-temporal-determinism-boundary.md. AgentRunWorkflow (workflows/agent_run.py) never
performs a write directly; it only calls these Activities and applies their typed result.
"""
from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any

from temporalio import activity
from temporalio.exceptions import ApplicationError

from aeon_worker.idempotency import EffectsLedger, derive_idempotency_key

DEFAULT_LEDGER_PATH = os.environ.get("AEON_EFFECTS_LEDGER_PATH", "/tmp/aeon_effects_ledger.json")

# TOOL-004. Two execution modes, and neither of them is implicit:
#
#   AEON_TOOLGW_ADDR                      -> the real Tool Gateway: Cedar policy applied before
#                                            anything runs, the tool actually executes, and
#                                            TOOL-005's Postgres table deduplicates by key.
#   AEON_TOOL_EXECUTION_MODE=local-ledger -> the file-backed ledger, which executes NOTHING.
#
# Falling back silently to the ledger when no gateway is configured is the exact shape this feature
# removes: it makes a misconfigured deployment indistinguishable from a working one, because every
# tool call "succeeds" and nothing happens. An unconfigured worker raises instead.
TOOLGW_ADDR = os.environ.get("AEON_TOOLGW_ADDR", "")
EXECUTION_MODE = os.environ.get("AEON_TOOL_EXECUTION_MODE", "")


@dataclass
class ExecuteToolInput:
    run_id: str
    node_id: str
    step_seq: int
    tool_name: str
    tool_args: dict[str, Any]
    # The principal the Cedar policy is evaluated against. The worker had no agent identity at all
    # until TOOL-004, which meant the per-agent policy this platform advertises could not be applied
    # from here even in principle: a call with no principal is a call no policy can deny.
    agent_manifest_ref: str = ""
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


async def execute_tool(inp: ExecuteToolInput) -> ExecuteToolOutput:
    """Executes a tool call with idempotency (RUN-004) — a plain function, not an activity
    definition, so it can be called directly from another Activity's own body (e.g.
    aeon_worker.activities.deep_research_activities' per-subtask Researcher Activity) without
    nesting a Temporal activity call inside an activity. execute_tool_activity below is the
    workflow-callable wrapper.

    With AEON_TOOLGW_ADDR set this delegates to the real Tool Gateway, which is the only mode where
    a tool actually runs. The `local-ledger` mode records the intent in a file and executes nothing;
    it exists so the crash/resume integration tests can prove their property with no Go services
    running, and it is opt-in precisely because a worker that took it silently would make every call
    look successful while the agent had no hands at all.
    """
    key = derive_idempotency_key(inp.run_id, inp.node_id, inp.step_seq, inp.tool_args)

    if TOOLGW_ADDR and EXECUTION_MODE == "local-ledger":
        # Both configured is a contradiction, not a precedence question. Picking one silently is how
        # a deployment ends up on the ledger with a gateway address sitting right there in its
        # environment — which is exactly the state this worker was found in: the compose stack had
        # AEON_TOOLGW_ADDR set and the code never read it.
        raise ApplicationError(
            "both AEON_TOOLGW_ADDR and AEON_TOOL_EXECUTION_MODE=local-ledger are set: these select "
            "different execution backends and only one can be meant. Unset whichever is wrong.",
            non_retryable=True,
        )

    if TOOLGW_ADDR:
        return await _execute_through_gateway(inp, key)
    if EXECUTION_MODE != "local-ledger":
        raise ApplicationError(
            "no tool execution backend configured: set AEON_TOOLGW_ADDR to reach the Tool Gateway, "
            "or AEON_TOOL_EXECUTION_MODE=local-ledger for the test-only ledger that executes "
            "nothing. Refusing to guess, because guessing wrong looks exactly like success.",
            non_retryable=True,
        )

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


async def _execute_through_gateway(inp: ExecuteToolInput, key: str) -> ExecuteToolOutput:
    """POSTs to the Tool Gateway's /execute, mapping its answers onto Temporal's retry semantics.

    That mapping is the part worth reading. A policy denial is NOT retryable: a tool outside the
    manifest will not become allowed by trying again, and retrying only delays a run that is already
    wrong. An in-flight rejection IS retryable, because the only thing wrong with it is timing. And
    a key reused with different arguments is not retryable either — it means the key derivation is
    broken upstream, and no number of attempts fixes that.
    """
    if not inp.agent_manifest_ref:
        raise ApplicationError(
            f"tool {inp.tool_name!r} has no agent_manifest_ref: the Tool Gateway evaluates policy "
            "against a principal, and a call without one cannot be governed",
            non_retryable=True,
        )

    payload = json.dumps(
        {
            "agent_manifest_ref": inp.agent_manifest_ref,
            "tool_name": inp.tool_name,
            "args": inp.tool_args,
            "idempotency_key": key,
        }
    ).encode()
    request = urllib.request.Request(
        f"http://{TOOLGW_ADDR}/execute",
        data=payload,
        headers={"Content-Type": "application/json"},
    )

    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            body = json.loads(response.read())
    except urllib.error.HTTPError as err:
        detail = err.read().decode(errors="replace")
        if err.code == 403:
            raise ApplicationError(
                f"tool {inp.tool_name!r} denied by policy for {inp.agent_manifest_ref!r}: {detail}",
                non_retryable=True,
            ) from err
        if err.code == 409:
            if '"retryable":true' in detail.replace(" ", ""):
                raise ApplicationError(f"tool execution already in flight: {detail}") from err
            raise ApplicationError(
                f"idempotency key collision for {inp.tool_name!r}: {detail}", non_retryable=True
            ) from err
        raise ApplicationError(f"tool gateway returned {err.code}: {detail}") from err

    if inp.simulate_crash_after_write and not body.get("deduplicated", False):
        activity.logger.warning("simulate_crash_after_write=True: exiting process now")
        os._exit(1)  # noqa: SLF001 — deliberate hard kill, not a normal exception path.

    return ExecuteToolOutput(
        deduplicated=bool(body.get("deduplicated", False)),
        idempotency_key=key,
        result=body.get("result", {}),
    )


@activity.defn
async def execute_tool_activity(inp: ExecuteToolInput) -> ExecuteToolOutput:
    return await execute_tool(inp)
