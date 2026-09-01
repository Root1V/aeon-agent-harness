"""ClaudeAgentInteropWorkflow (INT-007): the thinnest possible durable wrapper around a Modo B
Activity — proves a real `claude_agent_sdk.ClaudeSDKClient` turn loop genuinely runs inside a
Temporal Activity, not in workflow code. Per docs/adr/0001, this workflow calls exactly one Activity
and returns its result; it never touches the CLI subprocess, Anthropic's API, or a tool itself.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from datetime import timedelta

from temporalio import workflow
from temporalio.common import RetryPolicy

with workflow.unsafe.imports_passed_through():
    from aeon_worker.activities.framework_adapter_activities import (
        ClaudeAgentInteropInput,
        ClaudeAgentInteropOutput,
        run_claude_agent_interop_activity,
    )


@dataclass
class ClaudeAgentInteropWorkflowInput:
    query: str


@dataclass
class ClaudeAgentInteropWorkflowResult:
    query: str
    final_output: str = ""
    tool_result: dict = field(default_factory=dict)


@workflow.defn
class ClaudeAgentInteropWorkflow:
    @workflow.run
    async def run(self, request: ClaudeAgentInteropWorkflowInput) -> ClaudeAgentInteropWorkflowResult:
        output: ClaudeAgentInteropOutput = await workflow.execute_activity(
            run_claude_agent_interop_activity,
            ClaudeAgentInteropInput(run_id=workflow.info().workflow_id, query=request.query),
            start_to_close_timeout=timedelta(seconds=120),
            retry_policy=RetryPolicy(maximum_attempts=3),
        )

        return ClaudeAgentInteropWorkflowResult(query=output.query, final_output=output.final_output, tool_result=output.tool_result)
