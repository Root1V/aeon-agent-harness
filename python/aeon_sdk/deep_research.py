"""aeon_sdk's Deep Research entrypoint (DX-001): start_deep_research_run connects to a real
Temporal server, starts DeepResearchWorkflow (aeon_worker.workflows.deep_research_run), and awaits
its result — the first real, durable, end-to-end path through DR-001..DR-005. This is what
examples/deep-research/run.py runs against.

Deliberately scoped to Deep Research specifically for now, not a fully generic start_run(manifest)
that resolves any AgentManifest's runtime/graph — see roadmap.md DX-001 and backlog.md.
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from typing import Any

from temporalio.client import Client

from aeon_worker.workflows.deep_research_run import DeepResearchWorkflow, DeepResearchWorkflowInput

DEFAULT_TASK_QUEUE = "aeon-agent-run"


@dataclass
class DeepResearchReport:
    run_id: str
    query: str
    report_text: str
    cited_claim_ids: list[str] = field(default_factory=list)
    sufficient: bool = False
    topics_to_replan: list[str] = field(default_factory=list)


async def start_deep_research_run(
    query: str,
    candidates: list[dict[str, Any]],
    *,
    model: str,
    temporal_address: str = "localhost:7233",
    task_queue: str = DEFAULT_TASK_QUEUE,
    data_sensitivity: str = "",
) -> DeepResearchReport:
    """Starts a real DeepResearchWorkflow execution and blocks until it completes. `candidates` is
    already-resolved Model Gateway routing (see aeon_sdk.model_policy.resolve_candidates) — this
    function doesn't know about AgentManifest/ModelPolicyBundle files itself, only the routing data
    they resolve to."""
    client = await Client.connect(temporal_address)
    run_id = f"deep-research-{uuid.uuid4().hex[:12]}"
    handle = await client.start_workflow(
        DeepResearchWorkflow.run,
        DeepResearchWorkflowInput(query=query, model=model, candidates=candidates, data_sensitivity=data_sensitivity),
        id=run_id,
        task_queue=task_queue,
    )
    result = await handle.result()
    return DeepResearchReport(
        run_id=run_id,
        query=result.query,
        report_text=result.report_text,
        cited_claim_ids=result.cited_claim_ids,
        sufficient=result.sufficient,
        topics_to_replan=result.topics_to_replan,
    )
