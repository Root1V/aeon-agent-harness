"""OpenAIAgentsInteropWorkflow (INT-005): the thinnest possible durable wrapper around a Modo B
Activity — proves a real `agents.Agent`/`agents.Runner` loop genuinely runs inside a Temporal
Activity, not in workflow code. Per docs/adr/0001, this workflow calls exactly one Activity and
returns its result; it never touches the agent, the Model Gateway, or a tool itself. Mirrors
aeon_worker.workflows.crewai_interop_run.CrewAIInteropWorkflow, minus `candidates`/
`data_sensitivity` — the OpenAI Agents SDK adapter goes through `aeon-modelgw`'s
`POST /v1/chat/completions` (INT-002), which resolves a capability profile name directly, so there
is no per-call candidate list to thread through here.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from datetime import timedelta

from temporalio import workflow
from temporalio.common import RetryPolicy

with workflow.unsafe.imports_passed_through():
    from aeon_worker.activities.framework_adapter_activities import (
        OpenAIAgentsInteropInput,
        OpenAIAgentsInteropOutput,
        run_openai_agents_interop_activity,
    )


@dataclass
class OpenAIAgentsInteropWorkflowInput:
    query: str
    model: str


@dataclass
class OpenAIAgentsInteropWorkflowResult:
    query: str
    final_output: str = ""
    tool_result: dict = field(default_factory=dict)


@workflow.defn
class OpenAIAgentsInteropWorkflow:
    @workflow.run
    async def run(self, request: OpenAIAgentsInteropWorkflowInput) -> OpenAIAgentsInteropWorkflowResult:
        output: OpenAIAgentsInteropOutput = await workflow.execute_activity(
            run_openai_agents_interop_activity,
            OpenAIAgentsInteropInput(run_id=workflow.info().workflow_id, query=request.query, model=request.model),
            start_to_close_timeout=timedelta(seconds=120),
            retry_policy=RetryPolicy(maximum_attempts=3),
        )

        return OpenAIAgentsInteropWorkflowResult(query=output.query, final_output=output.final_output, tool_result=output.tool_result)
