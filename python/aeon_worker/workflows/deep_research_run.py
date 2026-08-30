"""DeepResearchWorkflow (DX-001): the first real, durable, replayable execution of DR-001..DR-005's
pipeline — Planner -> Researchers (parallel) -> Sufficiency Gate -> Reporter -> Citation Verifier —
with every model/tool call going through a real Temporal Activity. Per
docs/adr/0001-temporal-determinism-boundary.md, this workflow itself never calls a model, a tool, or
generates a random id: DR-002's Researcher and DR-004's Reporter do their own decide()/execute_tool()
calls, but only ever bound here to workflow.execute_activity, never a direct network call from
workflow code. Everything this workflow touches directly (evaluate_sufficiency, verify_and_repair,
assembling Evidence Ledger entries from already-returned Activity results) is pure and
deterministic — see aeon_worker.activities.deep_research_activities for where the actual I/O lives.

Deliberately single-pass for this first version: if the Sufficiency Gate finds a subtask
insufficient, the run still produces a report from whatever evidence WAS allowed and reports
sufficient=False plus which coverage_topics would need another planning round — automatic
replanning is real future work (DR-003 already computes exactly what it would need), not something
silently skipped here.
"""
from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any

from temporalio import workflow
from temporalio.common import RetryPolicy

with workflow.unsafe.imports_passed_through():
    from aeon_evidence.ledger import EvidenceLedger
    from aeon_profiles.deep_research.citation_verifier import verify_and_repair
    from aeon_profiles.deep_research.planner import ResearchPlan, Subtask
    from aeon_profiles.deep_research.reporter import ReportDraft, select_allowed_claims
    from aeon_profiles.deep_research.researcher import ResearchResult, ToolCallRecord
    from aeon_profiles.deep_research.sufficiency_gate import evaluate_sufficiency
    from aeon_worker.activities.deep_research_activities import (
        AllowedClaim,
        BuildReportInput,
        BuildReportOutput,
        PlanResearchInput,
        PlanResearchOutput,
        ResearchSubtaskInput,
        ResearchSubtaskOutput,
        build_report_activity,
        plan_research_activity,
        research_subtask_activity,
    )
    from aeon_worker.activities.model_activities import DecideCandidate

_ACTIVITY_TIMEOUT = timedelta(seconds=120)
_RETRY_POLICY = RetryPolicy(maximum_attempts=3)


@dataclass
class DeepResearchWorkflowInput:
    query: str
    model: str
    candidates: list[dict[str, Any]]  # [{"provider": ..., "model": ..., "priority": ...}, ...]
    data_sensitivity: str = ""


@dataclass
class DeepResearchWorkflowResult:
    query: str
    report_text: str
    cited_claim_ids: list[str] = field(default_factory=list)
    sufficient: bool = False
    topics_to_replan: list[str] = field(default_factory=list)


@workflow.defn
class DeepResearchWorkflow:
    @workflow.run
    async def run(self, request: DeepResearchWorkflowInput) -> DeepResearchWorkflowResult:
        run_id = workflow.info().workflow_id
        candidates = [DecideCandidate(provider=c["provider"], model=c["model"], priority=c["priority"]) for c in request.candidates]

        plan_output: PlanResearchOutput = await workflow.execute_activity(
            plan_research_activity,
            PlanResearchInput(query=request.query, model=request.model, candidates=candidates, data_sensitivity=request.data_sensitivity),
            start_to_close_timeout=_ACTIVITY_TIMEOUT,
            retry_policy=_RETRY_POLICY,
        )

        # asyncio.gather over workflow.execute_activity calls: Temporal's documented pattern for
        # concurrent Activities inside a deterministic workflow (same pattern as
        # aeon_worker.graph._execute_parallel) — every subtask's Researcher runs concurrently.
        research_outputs: list[ResearchSubtaskOutput] = list(
            await asyncio.gather(
                *(
                    workflow.execute_activity(
                        research_subtask_activity,
                        ResearchSubtaskInput(
                            run_id=run_id, subtask=subtask, model=request.model, candidates=candidates, data_sensitivity=request.data_sensitivity
                        ),
                        start_to_close_timeout=_ACTIVITY_TIMEOUT,
                        retry_policy=_RETRY_POLICY,
                    )
                    for subtask in plan_output.subtasks
                )
            )
        )

        # From here on: pure, deterministic logic only — applying results Activities already
        # returned, never new I/O or randomness (docs/adr/0001).
        plan = _to_research_plan(plan_output)
        results = [_to_research_result(o) for o in research_outputs]
        ledger = _build_ledger(plan, results)

        decision = evaluate_sufficiency(plan, results, ledger)
        allowed_claims = select_allowed_claims(decision, ledger)

        report_output: BuildReportOutput = await workflow.execute_activity(
            build_report_activity,
            BuildReportInput(
                query=request.query,
                model=request.model,
                candidates=candidates,
                data_sensitivity=request.data_sensitivity,
                allowed_claims=[
                    AllowedClaim(claim_id=c["claim_id"], claim=c["claim"], quote=c["quote"], source_id=c["source_id"])
                    for c in allowed_claims
                ],
            ),
            start_to_close_timeout=_ACTIVITY_TIMEOUT,
            retry_policy=_RETRY_POLICY,
        )

        draft = ReportDraft(query=request.query, text=report_output.text, cited_claim_ids=report_output.cited_claim_ids)
        verification = verify_and_repair(draft, decision, ledger)

        return DeepResearchWorkflowResult(
            query=request.query,
            report_text=draft.text,
            cited_claim_ids=verification.cited_claim_ids,
            sufficient=decision.sufficient and verification.verified,
            topics_to_replan=decision.topics_to_replan,
        )


def _to_research_plan(plan_output: PlanResearchOutput) -> ResearchPlan:
    return ResearchPlan(
        query=plan_output.query,
        subtasks=[
            Subtask(
                id=s.id, description=s.description, coverage_topic=s.coverage_topic, max_tool_calls=s.max_tool_calls, max_model_calls=s.max_model_calls
            )
            for s in plan_output.subtasks
        ],
    )


def _to_research_result(output: ResearchSubtaskOutput) -> ResearchResult:
    return ResearchResult(
        subtask_id=output.subtask_id,
        messages=[],  # per-subtask transcripts aren't threaded back through the Activity boundary
        tool_calls=[ToolCallRecord(tool_name=tc.tool_name, args=tc.args, result=tc.result) for tc in output.tool_calls],
        finished_reason=output.finished_reason,
        final_message=output.final_message,
    )


def _build_ledger(plan: ResearchPlan, results: list[ResearchResult]) -> EvidenceLedger:
    """Turns each subtask's tool-call results into EvidencePacket entries — deterministic ids
    derived from (subtask_id, call index), same scheme as aeon_evalops.runner's offline harness.
    Never passes `contradicts=` (no contradiction detection is wired yet — see DR-003's own
    docstring), so EvidenceLedger.add never generates a random contradiction_group id here."""
    ledger = EvidenceLedger()
    for subtask, result in zip(plan.subtasks, results, strict=True):
        for i, call in enumerate(result.tool_calls):
            content = str(call.result.get("content", call.result))
            ledger.add(
                {
                    "claim_id": f"{subtask.id}-claim-{i}",
                    "subtopic_id": subtask.id,
                    "claim": content,
                    "quote": content,
                    "source_id": f"{subtask.id}-source-{i}",
                    "retrieved_at": workflow.now().isoformat(),
                    "source_quality": 0.8,
                    "confidence": 0.8,
                    "support": "SUPPORTS",
                }
            )
    return ledger
