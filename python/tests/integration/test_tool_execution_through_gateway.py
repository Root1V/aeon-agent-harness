"""TOOL-004: the worker executes tools through the real Tool Gateway, not a local file.

Until this, `execute_tool` wrote the *intent* of a tool call to a JSON file and returned
`{"status": "written"}`. Its own docstring said "in production this delegates to the Tool Gateway"
— and nothing did. The gateway existed, enforced Cedar policy and had tests; the worker simply
never called it, so every tool call in every run was a no-op that looked like a success. All five
framework adapters (LangGraph, CrewAI, OpenAI Agents, MAF, Claude Agent SDK) route through this
same function, so none of them executed anything either.

These tests run against a REAL aeon-toolgw with a real policy bundle and a real Postgres dedupe
table — see `make test-python-integration`. A fake would verify the shape of a request and nothing
about policy or deduplication, which is the entire point.
"""
from __future__ import annotations

import os
import uuid

import pytest

from aeon_worker.activities import tool_activities
from aeon_worker.activities.tool_activities import ExecuteToolInput, execute_tool

AGENT_REF = "deep-research-general@0.1.0"  # the principal examples/deep-research/policy_bundle.yaml permits


@pytest.fixture
def gateway(monkeypatch) -> str:
    addr = os.environ.get("AEON_TEST_TOOLGW_ADDR")
    if not addr:
        pytest.skip("AEON_TEST_TOOLGW_ADDR not set — see make test-python-integration")
    monkeypatch.setattr(tool_activities, "TOOLGW_ADDR", addr)
    monkeypatch.setattr(tool_activities, "EXECUTION_MODE", "")
    return addr


def _call(**overrides) -> ExecuteToolInput:
    base = {
        "run_id": f"tool004-{uuid.uuid4()}",
        "node_id": "n0",
        "step_seq": 1,
        "tool_name": "search.web",
        "tool_args": {"query": "estado del arte"},
        "agent_manifest_ref": AGENT_REF,
    }
    base.update(overrides)
    return ExecuteToolInput(**base)


@pytest.mark.asyncio
async def test_worker_tool_call_executes_through_the_gateway_and_deduplicates(gateway):
    """The permitted path: the tool really runs, and a repeat of the same step does not run it again.

    The "does not run again" half is asserted here through the gateway's own `deduplicated` answer
    rather than by counting executions, because the gateway is a separate process. The counting
    proof lives where it can be done honestly — TOOL-005's Go test drives a tool that increments a
    counter and shows the effect happens exactly once. This test's job is the integration: that the
    worker reaches the gateway at all, with a principal, and reads its answer correctly.
    """
    call = _call()

    first = await execute_tool(call)
    assert first.deduplicated is False
    # The result now comes from the Go executor, not from a file that records intentions. The old
    # ledger path answered {"status": "written"} without a "tool" key and without ever running.
    assert first.result.get("tool") == "search.web", first.result
    assert first.idempotency_key

    second = await execute_tool(call)
    assert second.deduplicated is True, "a repeat of the same step re-executed the tool"
    assert second.idempotency_key == first.idempotency_key
    assert second.result == first.result


@pytest.mark.asyncio
async def test_a_tool_outside_the_manifest_is_denied_on_the_real_path(gateway):
    """Policy is applied where the call actually happens, not only in a gateway test.

    `shell.exec` is forbidden for every principal by the checked-in policy bundle. The denial has to
    be non-retryable: a tool outside the manifest does not become permitted by trying again, and
    retrying would only spend a run's budget on a call that is already wrong.
    """
    with pytest.raises(Exception) as excinfo:
        await execute_tool(_call(tool_name="shell.exec", tool_args={"command": "echo hi"}))

    assert "denied by policy" in str(excinfo.value)
    assert getattr(excinfo.value, "non_retryable", False) is True


@pytest.mark.asyncio
async def test_a_call_without_a_principal_is_refused_rather_than_run_unattributed(gateway):
    """The worker had no agent identity until TOOL-004, so this case could not even be expressed.

    Defaulting a principal would be worse than having none: every per-agent rule in the bundle would
    then apply to whoever happened to be running, and a denial would look like an allow.
    """
    with pytest.raises(Exception) as excinfo:
        await execute_tool(_call(agent_manifest_ref=""))

    assert "agent_manifest_ref" in str(excinfo.value)
    assert getattr(excinfo.value, "non_retryable", False) is True


@pytest.mark.asyncio
async def test_contradictory_configuration_is_refused_instead_of_silently_picking_one(monkeypatch):
    """Both backends configured is a contradiction, and choosing one quietly is how this bug lived.

    The compose stack had AEON_TOOLGW_ADDR set on the worker for months while the code read the file
    ledger — declared wiring that did nothing, and no way to notice.
    """
    monkeypatch.setattr(tool_activities, "TOOLGW_ADDR", "toolgw:9403")
    monkeypatch.setattr(tool_activities, "EXECUTION_MODE", "local-ledger")

    with pytest.raises(Exception) as excinfo:
        await execute_tool(_call())

    assert "only one can be meant" in str(excinfo.value)


@pytest.mark.asyncio
async def test_an_unconfigured_worker_refuses_rather_than_running_nothing(monkeypatch):
    """No backend configured must fail loudly. Silently using the ledger makes a broken deployment
    indistinguishable from a working one: every call succeeds and the agent has no hands."""
    monkeypatch.setattr(tool_activities, "TOOLGW_ADDR", "")
    monkeypatch.setattr(tool_activities, "EXECUTION_MODE", "")

    with pytest.raises(Exception) as excinfo:
        await execute_tool(_call())

    assert "no tool execution backend configured" in str(excinfo.value)
