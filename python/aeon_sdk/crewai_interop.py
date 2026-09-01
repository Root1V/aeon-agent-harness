"""aeon_sdk's CrewAI interop entrypoint (INT-004): mirrors aeon_sdk.langgraph_interop's
start_langgraph_interop_run — connects to a real Temporal server, starts CrewAIInteropWorkflow, and
awaits its result. This is what examples/crewai-interop/run.py runs against.
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from typing import Any

from temporalio.client import Client

from aeon_worker.workflows.crewai_interop_run import CrewAIInteropWorkflow, CrewAIInteropWorkflowInput

DEFAULT_TASK_QUEUE = "aeon-agent-run"


@dataclass
class CrewAIInteropResult:
    run_id: str
    query: str
    plan: str = ""
    tool_result: dict = field(default_factory=dict)


async def start_crewai_interop_run(
    query: str,
    candidates: list[dict[str, Any]],
    *,
    model: str,
    temporal_address: str = "localhost:7233",
    task_queue: str = DEFAULT_TASK_QUEUE,
    data_sensitivity: str = "",
) -> CrewAIInteropResult:
    client = await Client.connect(temporal_address)
    run_id = f"crewai-interop-{uuid.uuid4().hex[:12]}"
    handle = await client.start_workflow(
        CrewAIInteropWorkflow.run,
        CrewAIInteropWorkflowInput(query=query, model=model, candidates=candidates, data_sensitivity=data_sensitivity),
        id=run_id,
        task_queue=task_queue,
    )
    result = await handle.result()
    return CrewAIInteropResult(run_id=run_id, query=result.query, plan=result.plan, tool_result=result.tool_result)
