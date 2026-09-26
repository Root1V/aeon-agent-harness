"""Activities that run DR-001..DR-005's Deep Research pipeline stages for real (DX-001): each one
is its own I/O boundary — a real HTTP call to the Model Gateway for every model decision, the same
idempotent tool-execution path RUN-004 uses for tool calls — per docs/adr/0001.
DeepResearchWorkflow (aeon_worker.workflows.deep_research_run) calls these via
workflow.execute_activity and only ever touches the purely deterministic parts of the pipeline
(DR-003's evaluate_sufficiency, DR-005's verify_and_repair, and assembling Evidence Ledger entries
from already-returned Activity results) directly in workflow code.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from temporalio import activity

from aeon_profiles.deep_research.planner import Planner, Subtask
from aeon_profiles.deep_research.reporter import Reporter
from aeon_profiles.deep_research.researcher import Researcher
from aeon_worker.activities.model_activities import DecideCandidate, DecideInput, call_model_gateway
from aeon_worker.activities.tool_activities import ExecuteToolInput, execute_tool


@dataclass
class PlanResearchInput:
    query: str
    model: str
    candidates: list[DecideCandidate]
    data_sensitivity: str = ""


@dataclass
class PlanResearchSubtask:
    id: str
    description: str
    coverage_topic: str
    max_tool_calls: int
    max_model_calls: int


@dataclass
class PlanResearchOutput:
    query: str
    subtasks: list[PlanResearchSubtask]


async def _decide_via_gateway(candidates: list[DecideCandidate], data_sensitivity: str, rendered_context: dict[str, Any]) -> dict[str, Any]:
    result = await call_model_gateway(
        DecideInput(candidates=candidates, rendered_context=rendered_context, data_sensitivity=data_sensitivity)
    )
    return result.output


@activity.defn
async def plan_research_activity(inp: PlanResearchInput) -> PlanResearchOutput:
    async def decide(rendered_context: dict[str, Any]) -> dict[str, Any]:
        return await _decide_via_gateway(inp.candidates, inp.data_sensitivity, rendered_context)

    plan = await Planner(model=inp.model).plan(inp.query, decide)
    return PlanResearchOutput(
        query=plan.query,
        subtasks=[
            PlanResearchSubtask(
                id=s.id,
                description=s.description,
                coverage_topic=s.coverage_topic,
                max_tool_calls=s.max_tool_calls,
                max_model_calls=s.max_model_calls,
            )
            for s in plan.subtasks
        ],
    )


@dataclass
class ResearchSubtaskInput:
    run_id: str
    subtask: PlanResearchSubtask
    model: str
    candidates: list[DecideCandidate]
    data_sensitivity: str = ""
    # MDL-015: the principal the Tool Gateway evaluates policy against. This field did not exist, so
    # a Researcher could not pass one, and TOOL-004 refuses a tool call with no principal rather than
    # running it unattributed — which meant this profile could only ever use the ledger that executes
    # nothing. Found running the pipeline against the real platform: DX-001 never hit it, because a
    # run whose tools do nothing still completes.
    agent_manifest_ref: str = ""
    # MDL-015: the manifest's own tools.allow list, so the Researcher's prompt names exactly the tools
    # policy permits. Empty means "no tools", which the prompt states outright instead of leaving the
    # model to guess that none exist.
    allowed_tools: list[str] = field(default_factory=list)


@dataclass
class ResearchToolCall:
    tool_name: str
    args: dict[str, Any]
    result: dict[str, Any]


@dataclass
class ResearchSubtaskOutput:
    subtask_id: str
    finished_reason: str
    final_message: str | None
    tool_calls: list[ResearchToolCall] = field(default_factory=list)


@activity.defn
async def research_subtask_activity(inp: ResearchSubtaskInput) -> ResearchSubtaskOutput:
    async def decide(rendered_context: dict[str, Any]) -> dict[str, Any]:
        return await _decide_via_gateway(inp.candidates, inp.data_sensitivity, rendered_context)

    step_seq = 0

    async def do_execute_tool(tool_name: str, args: dict[str, Any]) -> dict[str, Any]:
        nonlocal step_seq
        step_seq += 1
        output = await execute_tool(
            ExecuteToolInput(
                run_id=inp.run_id,
                node_id=inp.subtask.id,
                step_seq=step_seq,
                tool_name=tool_name,
                tool_args=args,
                agent_manifest_ref=inp.agent_manifest_ref,
            )
        )
        return output.result

    subtask = Subtask(
        id=inp.subtask.id,
        description=inp.subtask.description,
        coverage_topic=inp.subtask.coverage_topic,
        max_tool_calls=inp.subtask.max_tool_calls,
        max_model_calls=inp.subtask.max_model_calls,
    )
    result = await Researcher(subtask, inp.model, inp.allowed_tools).research(decide, do_execute_tool)

    return ResearchSubtaskOutput(
        subtask_id=result.subtask_id,
        finished_reason=result.finished_reason,
        final_message=result.final_message,
        tool_calls=[ResearchToolCall(tool_name=tc.tool_name, args=tc.args, result=tc.result) for tc in result.tool_calls],
    )


@dataclass
class AllowedClaim:
    claim_id: str
    claim: str
    quote: str
    source_id: str


@dataclass
class BuildReportInput:
    query: str
    model: str
    candidates: list[DecideCandidate]
    allowed_claims: list[AllowedClaim]
    data_sensitivity: str = ""


@dataclass
class BuildReportOutput:
    text: str
    cited_claim_ids: list[str]


@activity.defn
async def build_report_activity(inp: BuildReportInput) -> BuildReportOutput:
    async def decide(rendered_context: dict[str, Any]) -> dict[str, Any]:
        return await _decide_via_gateway(inp.candidates, inp.data_sensitivity, rendered_context)

    allowed_claims = [{"claim_id": c.claim_id, "claim": c.claim, "quote": c.quote, "source_id": c.source_id} for c in inp.allowed_claims]
    draft = await Reporter(model=inp.model).report(inp.query, allowed_claims, decide)
    return BuildReportOutput(text=draft.text, cited_claim_ids=draft.cited_claim_ids)
