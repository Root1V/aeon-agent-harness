"""MafInteropWorkflow (INT-006): the thinnest possible durable wrapper around a Modo B Activity —
proves a real `agent_framework.Agent` loop genuinely runs inside a Temporal Activity, not in
workflow code. Per docs/adr/0001, this workflow calls exactly one Activity and returns its result;
it never touches the agent, the Model Gateway, or a tool itself. Mirrors
aeon_worker.workflows.openai_agents_interop_run.OpenAIAgentsInteropWorkflow field-for-field — same
reason: the Microsoft Agent Framework adapter also goes through `aeon-modelgw`'s
`POST /v1/chat/completions` (INT-002) with a capability profile name, so there is no per-call
candidate list to thread through here either.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from datetime import timedelta

from temporalio import workflow
from temporalio.common import RetryPolicy

with workflow.unsafe.imports_passed_through():
    from aeon_worker.activities.framework_adapter_activities import MafInteropInput, MafInteropOutput, run_maf_interop_activity


@dataclass
class MafInteropWorkflowInput:
    query: str
    model: str


@dataclass
class MafInteropWorkflowResult:
    query: str
    final_output: str = ""
    tool_result: dict = field(default_factory=dict)


@workflow.defn
class MafInteropWorkflow:
    @workflow.run
    async def run(self, request: MafInteropWorkflowInput) -> MafInteropWorkflowResult:
        output: MafInteropOutput = await workflow.execute_activity(
            run_maf_interop_activity,
            MafInteropInput(run_id=workflow.info().workflow_id, query=request.query, model=request.model),
            start_to_close_timeout=timedelta(seconds=120),
            retry_policy=RetryPolicy(maximum_attempts=3),
        )

        return MafInteropWorkflowResult(query=output.query, final_output=output.final_output, tool_result=output.tool_result)
