"""aeon_sdk's Claude Agent SDK interop entrypoint (INT-007): mirrors
aeon_sdk.maf_interop.start_maf_interop_run — connects to a real Temporal server, starts
ClaudeAgentInteropWorkflow, and awaits its result. This is what
examples/claude-agent-sdk-interop/run.py runs against. Unlike the other interop entrypoints, there
is no `model`/`candidates` parameter — see aeon_adapters.claude_agent_sdk.adapter's module docstring
for why this framework's model calls aren't Aeon-routed.
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass, field

from temporalio.client import Client

from aeon_worker.workflows.claude_agent_interop_run import ClaudeAgentInteropWorkflow, ClaudeAgentInteropWorkflowInput

DEFAULT_TASK_QUEUE = "aeon-agent-run"


@dataclass
class ClaudeAgentInteropResult:
    run_id: str
    query: str
    final_output: str = ""
    tool_result: dict = field(default_factory=dict)


async def start_claude_agent_interop_run(
    query: str,
    *,
    temporal_address: str = "localhost:7233",
    task_queue: str = DEFAULT_TASK_QUEUE,
) -> ClaudeAgentInteropResult:
    client = await Client.connect(temporal_address)
    run_id = f"claude-agent-interop-{uuid.uuid4().hex[:12]}"
    handle = await client.start_workflow(
        ClaudeAgentInteropWorkflow.run,
        ClaudeAgentInteropWorkflowInput(query=query),
        id=run_id,
        task_queue=task_queue,
    )
    result = await handle.result()
    return ClaudeAgentInteropResult(run_id=run_id, query=result.query, final_output=result.final_output, tool_result=result.tool_result)
