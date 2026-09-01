"""aeon_sdk's OpenAI Agents SDK interop entrypoint (INT-005): mirrors
aeon_sdk.crewai_interop.start_crewai_interop_run — connects to a real Temporal server, starts
OpenAIAgentsInteropWorkflow, and awaits its result. This is what
examples/openai-agents-interop/run.py runs against.
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass, field

from temporalio.client import Client

from aeon_worker.workflows.openai_agents_interop_run import OpenAIAgentsInteropWorkflow, OpenAIAgentsInteropWorkflowInput

DEFAULT_TASK_QUEUE = "aeon-agent-run"


@dataclass
class OpenAIAgentsInteropResult:
    run_id: str
    query: str
    final_output: str = ""
    tool_result: dict = field(default_factory=dict)


async def start_openai_agents_interop_run(
    query: str,
    *,
    model: str,
    temporal_address: str = "localhost:7233",
    task_queue: str = DEFAULT_TASK_QUEUE,
) -> OpenAIAgentsInteropResult:
    client = await Client.connect(temporal_address)
    run_id = f"openai-agents-interop-{uuid.uuid4().hex[:12]}"
    handle = await client.start_workflow(
        OpenAIAgentsInteropWorkflow.run,
        OpenAIAgentsInteropWorkflowInput(query=query, model=model),
        id=run_id,
        task_queue=task_queue,
    )
    result = await handle.result()
    return OpenAIAgentsInteropResult(run_id=run_id, query=result.query, final_output=result.final_output, tool_result=result.tool_result)
