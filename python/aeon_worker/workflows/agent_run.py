"""AgentRunWorkflow: the deterministic core of a run (RUN-001/RUN-002/RUN-004).

Per docs/adr/0001-temporal-determinism-boundary.md: this workflow is deterministic-only. It never
calls a model provider or a tool directly — it only invokes Activities (aeon_worker.activities) and
applies their typed results. This minimal version executes a single tool-call node; the full graph
runtime (sequential/parallel/conditional/loop/subgraph/fan-in, RUN-002) is not yet implemented —
see roadmap.md, still `TODO`.
"""
from __future__ import annotations

from datetime import timedelta
from typing import Any

from temporalio import workflow
from temporalio.common import RetryPolicy

with workflow.unsafe.imports_passed_through():
    from aeon_worker.activities.tool_activities import ExecuteToolInput, ExecuteToolOutput, execute_tool_activity


@workflow.defn
class AgentRunWorkflow:
    """Runs a single tool-call node to completion, surviving a worker crash mid-Activity.

    This is intentionally the smallest possible slice of RUN-002's graph runtime: one node, one
    Activity call. It exists to make docs/adr/0001's claim testable
    (tests/integration/test_crash_resume.py), not to be the final graph executor.
    """

    def __init__(self) -> None:
        self._last_activity_result: dict[str, Any] | None = None

    @workflow.run
    async def run(self, request: dict[str, Any]) -> dict[str, Any]:
        inp = ExecuteToolInput(
            run_id=request["run_id"],
            node_id=request.get("node_id", "node-0"),
            step_seq=request.get("step_seq", 0),
            tool_name=request["tool_name"],
            tool_args=request.get("tool_args", {}),
            agent_manifest_ref=request.get("agent_manifest_ref", ""),
            simulate_crash_after_write=request.get("simulate_crash_after_write", False),
        )

        output: ExecuteToolOutput = await workflow.execute_activity(
            execute_tool_activity,
            inp,
            start_to_close_timeout=timedelta(seconds=10),
            # A short retry policy: on the worker-crash-simulated path, Temporal's task-heartbeat
            # timeout is what actually triggers redelivery to a fresh worker; this policy bounds
            # additional application-level retries so a genuinely broken tool fails fast instead
            # of consuming budget indefinitely (RUN-003).
            retry_policy=RetryPolicy(maximum_attempts=5),
        )

        self._last_activity_result = {
            "deduplicated": output.deduplicated,
            "idempotency_key": output.idempotency_key,
            "result": output.result,
        }
        return self._last_activity_result
