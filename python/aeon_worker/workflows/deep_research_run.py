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
    from aeon_worker.activities.artifact_activities import WriteArtifactInput, write_artifact_activity
    from aeon_worker.activities.memory_activities import (
        ReflectInput,
        ReflectOutput,
        WriteCandidatesInput,
        WriteCandidatesOutput,
        reflect_activity,
        write_memory_candidates_activity,
    )
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
    from aeon_worker.tenancy import TENANT_MEMO_KEY

_ACTIVITY_TIMEOUT = timedelta(seconds=120)
_RETRY_POLICY = RetryPolicy(maximum_attempts=3)


@dataclass
class DeepResearchWorkflowInput:
    query: str
    model: str
    candidates: list[dict[str, Any]]  # [{"provider": ..., "model": ..., "priority": ...}, ...]
    data_sensitivity: str = ""
    # MDL-015: threaded to the Researchers so their tool calls carry a principal the Tool Gateway can
    # apply policy to. Empty keeps the previous behaviour, which is the ledger-only path.
    agent_manifest_ref: str = ""
    allowed_tools: list[str] = field(default_factory=list)
    # MEM-003: whether to reflect on the finished run. OFF by default, and that is deliberate — it costs
    # an extra model call per run, and a caller that has not thought about memory should not silently
    # start paying for one.
    reflect: bool = False
    # tenant_id IS GONE FROM HERE, and it was never functional: the only thing that read it was
    # WriteCandidatesInput, whose own field had stopped travelling when VRT-AEON-005 T-1 removed
    # tenant_id from the request body. So a client could set it and nothing happened.
    #
    # IT IS REMOVED RATHER THAN IGNORED, because the moment the worker gained the entitlement to name
    # a tenant (MayActForTenants), a client-supplied tenant_id reaching the header would have been a
    # privilege escalation THROUGH THE REQUEST BODY: a run belonging to A, asking the worker to write
    # into B, honoured because the WORKER is entitled to B. Exactly the defect SEC-005 and the Memory
    # Store each paid for, re-entered through a field that had looked harmless while it was dead.
    #
    # The tenant now comes from the Temporal memo, which is set by the Run Controller from the
    # authenticated caller and which a client cannot set.


@dataclass
class DeepResearchWorkflowResult:
    query: str
    report_text: str
    cited_claim_ids: list[str] = field(default_factory=list)
    sufficient: bool = False
    topics_to_replan: list[str] = field(default_factory=list)
    # MEM-003. Both fields, not just the count: a zero with no note means reflection ran and proposed
    # nothing, and a zero WITH a note means it could not run. Collapsing them would make a
    # misconfigured deployment look like a run the model had no lessons from.
    memory_candidates_written: int = 0
    reflection_note: str = ""
    # TOOL-009: where the finished report was parked, so a later step — or a person — can fetch it by id
    # instead of through Temporal. Both fields for the same reason as the two above: an empty id with a
    # note means there was nowhere to park it, which is not the same as a run that produced no report.
    report_artifact_id: str = ""
    artifact_note: str = ""


@workflow.defn
class DeepResearchWorkflow:
    @workflow.run
    async def run(self, request: DeepResearchWorkflowInput) -> DeepResearchWorkflowResult:
        run_id = workflow.info().workflow_id
        # From the MEMO, never from `request` — see the note where tenant_id used to live. Deterministic
        # under ADR-001: the memo is in the workflow's start attributes, so a replay reads the same
        # value; the "" default lets a run started before the memo existed replay instead of raising.
        tenant = workflow.memo_value(TENANT_MEMO_KEY, "")
        candidates = [DecideCandidate(provider=c["provider"], model=c["model"], priority=c["priority"]) for c in request.candidates]

        plan_output: PlanResearchOutput = await workflow.execute_activity(
            plan_research_activity,
            PlanResearchInput(
                query=request.query, model=request.model, candidates=candidates,
                data_sensitivity=request.data_sensitivity,
                # OBS-003b: the run's own id and the agent it runs as, so its cost lands attributed.
                run_id=run_id, agent_manifest_ref=request.agent_manifest_ref, tenant=tenant,
            ),
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
                            run_id=run_id,
                            subtask=subtask,
                            model=request.model,
                            candidates=candidates,
                            data_sensitivity=request.data_sensitivity,
                            agent_manifest_ref=request.agent_manifest_ref,
                            tenant=tenant,
                            allowed_tools=request.allowed_tools,
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
                run_id=run_id,  # OBS-003b
                agent_manifest_ref=request.agent_manifest_ref,
                tenant=tenant,
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
        sufficient = decision.sufficient and verification.verified

        # MEM-003: reflect on the finished run and persist whatever it proposes as CANDIDATES.
        #
        # AFTER the report and verification, never before: reflection reads the run's OUTCOME, and a
        # candidate proposed from an unverified draft would be a memory grounded in claims the Citation
        # Verifier had not yet accepted.
        #
        # Both steps are best-effort and the workflow says so in its result. The report is the
        # deliverable and it is finished by the time this runs; failing the run because an optional
        # post-hoc step did not parse would throw away completed work. What is NOT acceptable is failing
        # silently, so reflection_note carries why nothing was written.
        reflection_note = ""
        candidates_written = 0
        if request.reflect:
            reflect_output: ReflectOutput = await workflow.execute_activity(
                reflect_activity,
                ReflectInput(
                    run_id=run_id,
                    query=request.query,
                    outcome="success" if sufficient else "failure",
                    report_text=draft.text,
                    # ONLY the claim_ids the verifier accepted. The Reflector rejects a candidate citing
                    # anything the run did not produce, and handing it the full ledger would let a memory
                    # be grounded in a claim the report itself was not allowed to cite.
                    evidence_refs=list(verification.cited_claim_ids),
                    model=request.model,
                    candidates=candidates,
                    data_sensitivity=request.data_sensitivity,
                    agent_manifest_ref=request.agent_manifest_ref,  # OBS-003b
                    tenant=tenant,
                ),
                start_to_close_timeout=_ACTIVITY_TIMEOUT,
                retry_policy=_RETRY_POLICY,
            )
            reflection_note = reflect_output.skipped_reason
            if reflect_output.candidates:
                write_output: WriteCandidatesOutput = await workflow.execute_activity(
                    write_memory_candidates_activity,
                    WriteCandidatesInput(
                        run_id=run_id,
                        tenant=tenant,
                        candidates=reflect_output.candidates,
                    ),
                    start_to_close_timeout=_ACTIVITY_TIMEOUT,
                    retry_policy=_RETRY_POLICY,
                )
                candidates_written = write_output.written
                if write_output.skipped_reason:
                    reflection_note = write_output.skipped_reason

        # TOOL-009: the report is parked as an artifact, AFTER the verification that decides what it is
        # allowed to contain. Writing it earlier would park a draft and report its id as the report's.
        #
        # One attempt and no retry policy: the deliverable is already produced and in the result below, so
        # a retry loop here would spend time after the work for a copy. The activity never raises — the
        # reason travels in `note` — so this needs no try/except of its own.
        artifact = await workflow.execute_activity(
            write_artifact_activity,
            # tenant= is what decides WHICH artifact store this report lands in (GOV-001g), and it
            # comes from the memo like everything else — never from the request.
            WriteArtifactInput(run_id=run_id, name="report.md", content=draft.text, tenant=tenant),
            start_to_close_timeout=_ACTIVITY_TIMEOUT,
            retry_policy=RetryPolicy(maximum_attempts=1),
        )

        return DeepResearchWorkflowResult(
            query=request.query,
            report_text=draft.text,
            cited_claim_ids=verification.cited_claim_ids,
            sufficient=sufficient,
            topics_to_replan=decision.topics_to_replan,
            memory_candidates_written=candidates_written,
            reflection_note=reflection_note,
            report_artifact_id=artifact.artifact_id,
            artifact_note=artifact.note,
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
