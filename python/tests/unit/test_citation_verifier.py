"""DR-005's acceptance test: a Reporter draft cannot get through with an invented citation —
either it's repaired to a real claim already backed by the draft's own text, or verification fails
(test_reporter_cannot_invent_citations).
"""
from __future__ import annotations

from aeon_evidence.ledger import EvidenceLedger
from aeon_profiles.deep_research.citation_verifier import verify_and_repair
from aeon_profiles.deep_research.reporter import ReportDraft
from aeon_profiles.deep_research.sufficiency_gate import CoverageEntry, SufficiencyDecision


def _claim(claim_id: str, subtopic_id: str, quote: str, source_id: str = "src-1", support: str = "SUPPORTS") -> dict:
    return {
        "claim_id": claim_id,
        "subtopic_id": subtopic_id,
        "claim": quote,
        "quote": quote,
        "source_id": source_id,
        "retrieved_at": "2026-08-29T00:00:00+00:00",
        "source_quality": 0.8,
        "confidence": 0.8,
        "support": support,
    }


def _sufficient_decision(subtask_id: str, topic: str) -> SufficiencyDecision:
    entry = CoverageEntry(subtask_id, topic, covered=True, contested=False, reason="sufficient")
    return SufficiencyDecision(sufficient=True, coverage=[entry], topics_to_replan=[])


def test_reporter_cannot_invent_citations_gets_repaired_when_the_text_matches_real_evidence():
    ledger = EvidenceLedger()
    real = ledger.add(_claim("real-claim", "st-A", "the transformer architecture was introduced in 2017"))
    decision = _sufficient_decision("st-A", "topic-A")

    draft = ReportDraft(
        query="q",
        text="the transformer architecture was introduced in 2017, which changed NLP research.",
        cited_claim_ids=["invented-claim-id"],
    )

    result = verify_and_repair(draft, decision, ledger)

    assert result.verified is True
    assert result.cited_claim_ids == [real["claim_id"]]
    assert len(result.issues) == 1
    assert result.issues[0].claim_id == "invented-claim-id"
    assert result.issues[0].repaired_with == real["claim_id"]


def test_reporter_cannot_invent_citations_fails_when_no_matching_evidence_exists():
    ledger = EvidenceLedger()
    ledger.add(_claim("real-claim", "st-A", "the transformer architecture was introduced in 2017"))
    decision = _sufficient_decision("st-A", "topic-A")

    draft = ReportDraft(query="q", text="completely unrelated prose with no citation support at all", cited_claim_ids=["invented-claim-id"])

    result = verify_and_repair(draft, decision, ledger)

    assert result.verified is False
    assert result.cited_claim_ids == []
    assert result.issues[0].claim_id == "invented-claim-id"
    assert result.issues[0].repaired_with is None


def test_citation_verifier_passes_a_genuinely_allowed_citation_through_untouched():
    ledger = EvidenceLedger()
    real = ledger.add(_claim("real-claim", "st-A", "a real, cited fact"))
    decision = _sufficient_decision("st-A", "topic-A")

    draft = ReportDraft(query="q", text="a real, cited fact, reported faithfully.", cited_claim_ids=[real["claim_id"]])

    result = verify_and_repair(draft, decision, ledger)

    assert result.verified is True
    assert result.cited_claim_ids == [real["claim_id"]]
    assert result.issues == []


def test_citation_verifier_treats_a_gate_excluded_citation_as_unrepairable_when_text_does_not_match():
    """A claim_id that genuinely exists in the ledger but belongs to a contested subtask is still
    not citable — the Sufficiency Gate's exclusion holds even on this second, independent pass."""
    ledger = EvidenceLedger()
    contested_a = ledger.add(_claim("contested-a", "st-contested", "a disputed fact"))
    ledger.add(_claim("contested-b", "st-contested", "the opposite disputed fact", support="CONTRADICTS"), contradicts=[contested_a["claim_id"]])

    decision = SufficiencyDecision(
        sufficient=False,
        coverage=[CoverageEntry("st-contested", "topic-contested", covered=True, contested=True, reason="unresolved_contradiction")],
        topics_to_replan=["topic-contested"],
    )

    draft = ReportDraft(query="q", text="a disputed fact, stated as settled.", cited_claim_ids=[contested_a["claim_id"]])
    result = verify_and_repair(draft, decision, ledger)

    assert result.verified is False
    assert result.issues[0].claim_id == contested_a["claim_id"]
    assert result.issues[0].repaired_with is None


def test_citation_verifier_repairs_multiple_invented_citations_independently():
    ledger = EvidenceLedger()
    claim_x = ledger.add(_claim("x", "st-A", "fact X is true"))
    claim_y = ledger.add(_claim("y", "st-A", "fact Y is also true"))
    decision = _sufficient_decision("st-A", "topic-A")

    draft = ReportDraft(
        query="q",
        text="fact X is true. Separately, fact Y is also true.",
        cited_claim_ids=["invented-1", "invented-2"],
    )

    result = verify_and_repair(draft, decision, ledger)

    assert result.verified is True
    assert set(result.cited_claim_ids) == {claim_x["claim_id"], claim_y["claim_id"]}
    assert {i.repaired_with for i in result.issues} == {claim_x["claim_id"], claim_y["claim_id"]}
