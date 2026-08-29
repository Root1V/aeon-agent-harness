"""GraphRunWorkflow: RUN-002's Graph Runtime entrypoint, and RUN-001's Run Controller target.
Executes a GraphNode (proto/schemas/graph_spec.schema.json) to completion via
aeon_worker.graph.execute_graph.

Deterministic per docs/adr/0001-temporal-determinism-boundary.md: every non-deterministic
operation happens inside execute_graph's Activity calls, never here. This supersedes
AgentRunWorkflow (workflows/agent_run.py) as the general-purpose entrypoint; AgentRunWorkflow stays
as-is because tests/integration/test_crash_resume.py pins its exact single-node shape.

RUN-001 control surface: `pause`/`resume` signals and an `is_paused` query, driven by
go/internal/runcontroller (aeon-runcontroller). start/cancel/status/stream need no workflow-side
code at all — they're native Temporal client operations (StartWorkflow, CancelWorkflow,
DescribeWorkflowExecution) the Run Controller calls directly.
"""
from __future__ import annotations

from typing import Any

from temporalio import workflow

with workflow.unsafe.imports_passed_through():
    # aeon_worker.activities.tool_activities is imported here too (not just inside
    # aeon_worker.graph) even though this file never uses ExecuteToolInput/Output/
    # execute_tool_activity directly. Without this, Temporal's sandbox reload of THIS file (the
    # one carrying @workflow.defn) does not register tool_activities as passthrough for the
    # nested import inside aeon_worker.graph, and Activity result decoding fails deterministically
    # with "name 'Any' is not defined" the first time execute_tool_activity's dataclass return type
    # is resolved (see graph.py's module docstring and docs/adr/0001).
    from aeon_worker.activities.tool_activities import ExecuteToolInput, ExecuteToolOutput, execute_tool_activity
    from aeon_worker.graph import GraphExecutionState, execute_graph

_ = (ExecuteToolInput, ExecuteToolOutput, execute_tool_activity)  # imported for their passthrough side effect only


@workflow.defn
class GraphRunWorkflow:
    def __init__(self) -> None:
        self._paused = False

    @workflow.signal
    async def pause(self) -> None:
        self._paused = True

    @workflow.signal
    async def resume(self) -> None:
        self._paused = False

    @workflow.query
    def is_paused(self) -> bool:
        return self._paused

    @workflow.run
    async def run(self, request: dict[str, Any]) -> dict[str, Any]:
        state = GraphExecutionState(run_id=request["run_id"], is_paused=lambda: self._paused)
        result = await execute_graph(request["graph"], state)
        return {"run_id": request["run_id"], "result": result}
