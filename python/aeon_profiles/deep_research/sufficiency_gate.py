"""Sufficiency Gate (DR-003): reviews DR-002's ResearchResults against DR-001's coverage plan and
the Evidence Ledger's (RAG-003) contradiction groups, and decides whether the research as a whole is
sufficient to hand to a Reporter (DR-004) — or which coverage topics must be replanned first.

Evidence is linked to a subtask via EvidencePacket.subtopic_id == Subtask.id (proto/schemas/
evidence_packet.schema.json's subtopic_id is documented as format "uuid", but nothing in this
codebase enforces that format assertion — see aeon_worker.decision's decision_id for the same
precedent — so a Subtask's plain string id is a valid subtopic_id here).

This gate never resolves a contradiction itself, matching the Ledger's own stated non-goal
(aeon_evidence.ledger's docstring): an unresolved contradiction_group on a subtask's evidence is a
REASON to request that topic be replanned, never something this module silently picks a side on.
"""
from __future__ import annotations

from dataclasses import dataclass, field

from aeon_evidence.ledger import EvidenceLedger
from aeon_profiles.deep_research.planner import ResearchPlan
from aeon_profiles.deep_research.researcher import ResearchResult

# ResearchResult.finished_reason values that mean a subtask never actually completed its work —
# see researcher.py's Researcher.research, which is the only producer of these values.
INCOMPLETE_REASONS = frozenset({"budget_exhausted", "replan_requested"})

SUPPORTING_KINDS = frozenset({"SUPPORTS", "CONTEXT"})


@dataclass
class CoverageEntry:
    subtask_id: str
    coverage_topic: str
    covered: bool
    contested: bool
    reason: str  # "sufficient" | "incomplete_research" | "no_supporting_evidence" | "unresolved_contradiction"


@dataclass
class SufficiencyDecision:
    sufficient: bool
    coverage: list[CoverageEntry]
    # coverage_topic values that need another planning round — either uncovered, or covered but
    # still contested by an unresolved contradiction. Empty exactly when sufficient is True.
    topics_to_replan: list[str] = field(default_factory=list)


def evaluate_sufficiency(plan: ResearchPlan, results: list[ResearchResult], ledger: EvidenceLedger) -> SufficiencyDecision:
    results_by_subtask = {r.subtask_id: r for r in results}
    coverage: list[CoverageEntry] = []

    for subtask in plan.subtasks:
        result = results_by_subtask.get(subtask.id)

        if result is None or result.finished_reason in INCOMPLETE_REASONS:
            coverage.append(
                CoverageEntry(subtask.id, subtask.coverage_topic, covered=False, contested=False, reason="incomplete_research")
            )
            continue

        packets = [p for p in ledger.all() if p.get("subtopic_id") == subtask.id]
        supporting = [p for p in packets if p.get("support") in SUPPORTING_KINDS]
        if not supporting:
            coverage.append(
                CoverageEntry(subtask.id, subtask.coverage_topic, covered=False, contested=False, reason="no_supporting_evidence")
            )
            continue

        if any(p.get("contradiction_group") for p in packets):
            coverage.append(
                CoverageEntry(subtask.id, subtask.coverage_topic, covered=True, contested=True, reason="unresolved_contradiction")
            )
            continue

        coverage.append(CoverageEntry(subtask.id, subtask.coverage_topic, covered=True, contested=False, reason="sufficient"))

    topics_to_replan = [c.coverage_topic for c in coverage if not c.covered or c.contested]
    return SufficiencyDecision(sufficient=not topics_to_replan, coverage=coverage, topics_to_replan=topics_to_replan)
