"""LangGraphInteropWorkflow (INT-001): the thinnest possible durable wrapper around a Modo B
Activity — proves a real, compiled LangGraph graph genuinely runs inside a Temporal Activity, not
in workflow code. Per docs/adr/0001, this workflow calls exactly one Activity and returns its
result; it never touches the graph, the Model Gateway, or a tool itself.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any

from temporalio import workflow
from temporalio.common import RetryPolicy

with workflow.unsafe.imports_passed_through():
    from aeon_worker.activities.framework_adapter_activities import (
        LangGraphInteropInput,
        LangGraphInteropOutput,
        run_langgraph_interop_activity,
    )
    from aeon_worker.activities.model_activities import DecideCandidate


@dataclass
class LangGraphInteropWorkflowInput:
    query: str
    model: str
    candidates: list[dict[str, Any]]
    data_sensitivity: str = ""


@dataclass
class LangGraphInteropWorkflowResult:
    query: str
    plan: str = ""
    tool_result: dict = field(default_factory=dict)


@workflow.defn
class LangGraphInteropWorkflow:
    @workflow.run
    async def run(self, request: LangGraphInteropWorkflowInput) -> LangGraphInteropWorkflowResult:
        candidates = [DecideCandidate(provider=c["provider"], model=c["model"], priority=c["priority"]) for c in request.candidates]

        output: LangGraphInteropOutput = await workflow.execute_activity(
            run_langgraph_interop_activity,
            LangGraphInteropInput(
                run_id=workflow.info().workflow_id,
                query=request.query,
                model=request.model,
                candidates=candidates,
                data_sensitivity=request.data_sensitivity,
            ),
            start_to_close_timeout=timedelta(seconds=120),
            retry_policy=RetryPolicy(maximum_attempts=3),
        )

        return LangGraphInteropWorkflowResult(query=output.query, plan=output.plan, tool_result=output.tool_result)
