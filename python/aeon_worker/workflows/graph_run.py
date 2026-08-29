"""GraphRunWorkflow: RUN-002's Graph Runtime entrypoint. Executes a GraphNode
(proto/schemas/graph_spec.schema.json) to completion via aeon_worker.graph.execute_graph.

Deterministic per docs/adr/0001-temporal-determinism-boundary.md: every non-deterministic
operation happens inside execute_graph's Activity calls, never here. This supersedes
AgentRunWorkflow (workflows/agent_run.py) as the general-purpose entrypoint; AgentRunWorkflow stays
as-is because tests/integration/test_crash_resume.py pins its exact single-node shape.
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
    @workflow.run
    async def run(self, request: dict[str, Any]) -> dict[str, Any]:
        state = GraphExecutionState(run_id=request["run_id"])
        result = await execute_graph(request["graph"], state)
        return {"run_id": request["run_id"], "result": result}
