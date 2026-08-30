"""DR-003's acceptance test: the Sufficiency Gate must request a replan — never silently proceed —
when a subtask's research is incomplete, produced no supporting evidence, or is contested by an
unresolved contradiction (test_sufficiency_gate_replans and friends).
"""
from __future__ import annotations

import uuid

from aeon_evidence.ledger import EvidenceLedger
from aeon_profiles.deep_research.planner import ResearchPlan, Subtask
from aeon_profiles.deep_research.researcher import ResearchResult
from aeon_profiles.deep_research.sufficiency_gate import evaluate_sufficiency


def _subtask(id_: str, topic: str) -> Subtask:
    return Subtask(id=id_, description=f"Investigate {topic}", coverage_topic=topic, max_tool_calls=5, max_model_calls=5)


def _result(subtask_id: str, finished_reason: str = "finish") -> ResearchResult:
    return ResearchResult(subtask_id=subtask_id, messages=[], finished_reason=finished_reason, final_message="done")


def _packet(subtopic_id: str, source_id: str, support: str = "SUPPORTS") -> dict:
    return {
        "claim_id": str(uuid.uuid4()),
        "subtopic_id": subtopic_id,
        "claim": f"a claim about {subtopic_id}",
        "quote": f"quote for {subtopic_id}/{source_id}",
        "source_id": source_id,
        "retrieved_at": "2026-08-29T00:00:00+00:00",
        "source_quality": 0.8,
        "confidence": 0.8,
        "support": support,
    }


def test_sufficiency_gate_accepts_a_fully_covered_uncontested_plan():
    plan = ResearchPlan(query="q", subtasks=[_subtask("st-A", "topic-A"), _subtask("st-B", "topic-B")])
    results = [_result("st-A"), _result("st-B")]
    ledger = EvidenceLedger()
    ledger.add(_packet("st-A", "src-1"))
    ledger.add(_packet("st-B", "src-2"))

    decision = evaluate_sufficiency(plan, results, ledger)

    assert decision.sufficient is True
    assert decision.topics_to_replan == []
    assert all(c.covered and not c.contested for c in decision.coverage)


def test_sufficiency_gate_replans_a_topic_whose_researcher_hit_its_budget():
    plan = ResearchPlan(query="q", subtasks=[_subtask("st-A", "topic-A"), _subtask("st-B", "topic-B")])
    results = [_result("st-A"), _result("st-B", finished_reason="budget_exhausted")]
    ledger = EvidenceLedger()
    ledger.add(_packet("st-A", "src-1"))

    decision = evaluate_sufficiency(plan, results, ledger)

    assert decision.sufficient is False
    assert decision.topics_to_replan == ["topic-B"]
    contested_entry = next(c for c in decision.coverage if c.subtask_id == "st-B")
    assert contested_entry.reason == "incomplete_research"
    assert contested_entry.covered is False


def test_sufficiency_gate_replans_a_topic_that_requested_replan_itself():
    plan = ResearchPlan(query="q", subtasks=[_subtask("st-A", "topic-A")])
    results = [_result("st-A", finished_reason="replan_requested")]
    decision = evaluate_sufficiency(plan, results, EvidenceLedger())

    assert decision.sufficient is False
    assert decision.topics_to_replan == ["topic-A"]


def test_sufficiency_gate_replans_a_finished_topic_with_no_supporting_evidence():
    """A Researcher can call FINISH without ever having produced usable evidence (e.g. every tool
    call came back empty) — the gate must not treat "finished" alone as "covered"."""
    plan = ResearchPlan(query="q", subtasks=[_subtask("st-A", "topic-A")])
    results = [_result("st-A")]
    decision = evaluate_sufficiency(plan, results, EvidenceLedger())

    assert decision.sufficient is False
    assert decision.topics_to_replan == ["topic-A"]
    assert decision.coverage[0].reason == "no_supporting_evidence"


def test_sufficiency_gate_replans_a_covered_topic_contested_by_an_unresolved_contradiction():
    plan = ResearchPlan(query="q", subtasks=[_subtask("st-A", "topic-A")])
    results = [_result("st-A")]
    ledger = EvidenceLedger()
    claim_a = ledger.add(_packet("st-A", "src-1"))
    ledger.add(_packet("st-A", "src-2", support="CONTRADICTS"), contradicts=[claim_a["claim_id"]])

    decision = evaluate_sufficiency(plan, results, ledger)

    assert decision.sufficient is False
    assert decision.topics_to_replan == ["topic-A"]
    entry = decision.coverage[0]
    assert entry.covered is True, "contested evidence still counts as covered, just not sufficient"
    assert entry.contested is True
    assert entry.reason == "unresolved_contradiction"


def test_sufficiency_gate_ignores_a_contradiction_group_on_a_different_subtask():
    """Isolation carries through from DR-002: a contradiction on one subtask's evidence must never
    contest a sibling subtask that has none."""
    plan = ResearchPlan(query="q", subtasks=[_subtask("st-A", "topic-A"), _subtask("st-B", "topic-B")])
    results = [_result("st-A"), _result("st-B")]
    ledger = EvidenceLedger()
    claim_a = ledger.add(_packet("st-A", "src-1"))
    ledger.add(_packet("st-A", "src-2", support="CONTRADICTS"), contradicts=[claim_a["claim_id"]])
    ledger.add(_packet("st-B", "src-3"))

    decision = evaluate_sufficiency(plan, results, ledger)

    assert decision.topics_to_replan == ["topic-A"]
    entry_b = next(c for c in decision.coverage if c.subtask_id == "st-B")
    assert entry_b.covered is True
    assert entry_b.contested is False


def test_sufficiency_gate_treats_a_missing_result_as_incomplete():
    """A subtask the plan named but that never even produced a ResearchResult (e.g. a crashed
    Researcher) must not be silently skipped — it's exactly as incomplete as one that ran out of
    budget."""
    plan = ResearchPlan(query="q", subtasks=[_subtask("st-A", "topic-A")])
    decision = evaluate_sufficiency(plan, results=[], ledger=EvidenceLedger())

    assert decision.sufficient is False
    assert decision.coverage[0].reason == "incomplete_research"
