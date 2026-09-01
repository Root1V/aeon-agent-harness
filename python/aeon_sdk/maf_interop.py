"""aeon_sdk's Microsoft Agent Framework interop entrypoint (INT-006): mirrors
aeon_sdk.openai_agents_interop.start_openai_agents_interop_run — connects to a real Temporal
server, starts MafInteropWorkflow, and awaits its result. This is what
examples/maf-interop/run.py runs against.
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass, field

from temporalio.client import Client

from aeon_worker.workflows.maf_interop_run import MafInteropWorkflow, MafInteropWorkflowInput

DEFAULT_TASK_QUEUE = "aeon-agent-run"


@dataclass
class MafInteropResult:
    run_id: str
    query: str
    final_output: str = ""
    tool_result: dict = field(default_factory=dict)


async def start_maf_interop_run(
    query: str,
    *,
    model: str,
    temporal_address: str = "localhost:7233",
    task_queue: str = DEFAULT_TASK_QUEUE,
) -> MafInteropResult:
    client = await Client.connect(temporal_address)
    run_id = f"maf-interop-{uuid.uuid4().hex[:12]}"
    handle = await client.start_workflow(
        MafInteropWorkflow.run,
        MafInteropWorkflowInput(query=query, model=model),
        id=run_id,
        task_queue=task_queue,
    )
    result = await handle.result()
    return MafInteropResult(run_id=run_id, query=result.query, final_output=result.final_output, tool_result=result.tool_result)
