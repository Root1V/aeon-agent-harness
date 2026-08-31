"""aeon_sdk's LangGraph interop entrypoint (INT-001): mirrors aeon_sdk.deep_research's
start_deep_research_run — connects to a real Temporal server, starts LangGraphInteropWorkflow, and
awaits its result. This is what examples/langgraph-interop/run.py runs against.
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from typing import Any

from temporalio.client import Client

from aeon_worker.workflows.langgraph_interop_run import LangGraphInteropWorkflow, LangGraphInteropWorkflowInput

DEFAULT_TASK_QUEUE = "aeon-agent-run"


@dataclass
class LangGraphInteropResult:
    run_id: str
    query: str
    plan: str = ""
    tool_result: dict = field(default_factory=dict)


async def start_langgraph_interop_run(
    query: str,
    candidates: list[dict[str, Any]],
    *,
    model: str,
    temporal_address: str = "localhost:7233",
    task_queue: str = DEFAULT_TASK_QUEUE,
    data_sensitivity: str = "",
) -> LangGraphInteropResult:
    client = await Client.connect(temporal_address)
    run_id = f"langgraph-interop-{uuid.uuid4().hex[:12]}"
    handle = await client.start_workflow(
        LangGraphInteropWorkflow.run,
        LangGraphInteropWorkflowInput(query=query, model=model, candidates=candidates, data_sensitivity=data_sensitivity),
        id=run_id,
        task_queue=task_queue,
    )
    result = await handle.result()
    return LangGraphInteropResult(run_id=run_id, query=result.query, plan=result.plan, tool_result=result.tool_result)
