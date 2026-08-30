"""test_ledger_contradiction_grouping — the acceptance test named in roadmap.md RAG-003.

Proves contradictions are preserved and grouped, not silently resolved: two claims a caller
identifies as contradicting each other end up in the same contradiction_group, a third claim named
against either of them joins that SAME group rather than fragmenting into a new one, and dedupe/
source-quality aggregation are real, not just present in the API surface.
"""
from __future__ import annotations

import uuid

import pytest

from aeon_evidence.ledger import EvidenceLedger, UnknownClaimError


def _packet(claim: str, quote: str, source_id: str, support: str = "SUPPORTS", source_quality: float = 0.8) -> dict:
    return {
        "claim_id": str(uuid.uuid4()),
        "subtopic_id": str(uuid.uuid4()),
        "claim": claim,
        "quote": quote,
        "source_id": source_id,
        "retrieved_at": "2026-08-29T00:00:00+00:00",
        "source_quality": source_quality,
        "confidence": 0.8,
        "support": support,
    }


def test_ledger_contradiction_grouping_links_two_contradicting_claims():
    ledger = EvidenceLedger()
    claim_a = ledger.add(_packet("the sky is blue", "the sky is blue", source_id="s1"))
    claim_b = ledger.add(
        _packet("the sky is not blue", "the sky is not blue", source_id="s2", support="CONTRADICTS"),
        contradicts=[claim_a["claim_id"]],
    )

    assert claim_a["claim_id"] != claim_b["claim_id"]
    group_id = claim_b["contradiction_group"]
    assert group_id is not None
    assert ledger.get(claim_a["claim_id"])["contradiction_group"] == group_id

    group = ledger.contradiction_group(group_id)
    assert {p["claim_id"] for p in group} == {claim_a["claim_id"], claim_b["claim_id"]}


def test_ledger_contradiction_grouping_a_third_claim_joins_the_existing_group():
    ledger = EvidenceLedger()
    claim_a = ledger.add(_packet("X is true", "X is true", source_id="s1"))
    claim_b = ledger.add(_packet("X is false", "X is false", source_id="s2", support="CONTRADICTS"), contradicts=[claim_a["claim_id"]])
    claim_c = ledger.add(
        _packet("X is uncertain", "X is uncertain", source_id="s3", support="CONTEXT"),
        contradicts=[claim_a["claim_id"], claim_b["claim_id"]],
    )

    group_id = claim_a_group = ledger.get(claim_a["claim_id"])["contradiction_group"]
    assert ledger.get(claim_b["claim_id"])["contradiction_group"] == group_id
    assert claim_c["contradiction_group"] == group_id, "a third claim naming both existing members must join their SAME group, not a new one"

    group = ledger.contradiction_group(group_id)
    assert {p["claim_id"] for p in group} == {claim_a["claim_id"], claim_b["claim_id"], claim_c["claim_id"]}


def test_ledger_contradiction_grouping_unrelated_claims_stay_ungrouped():
    ledger = EvidenceLedger()
    claim = ledger.add(_packet("unrelated claim", "unrelated claim", source_id="s1"))
    assert claim.get("contradiction_group") is None


def test_ledger_contradiction_grouping_rejects_unknown_claim_id():
    ledger = EvidenceLedger()
    with pytest.raises(UnknownClaimError):
        ledger.add(_packet("X", "X", source_id="s1"), contradicts=["not-a-real-claim-id"])


def test_ledger_dedupes_identical_source_and_quote():
    ledger = EvidenceLedger()
    packet = _packet("dup claim", "dup quote", source_id="s1")
    first = ledger.add(packet)
    second = ledger.add(dict(packet))  # a fresh dict, but same source_id+quote

    assert first["claim_id"] == second["claim_id"]
    assert len(ledger.all()) == 1


def test_ledger_source_quality_averages_across_packets_from_the_same_source():
    ledger = EvidenceLedger()
    ledger.add(_packet("claim 1", "quote 1", source_id="s1", source_quality=1.0))
    ledger.add(_packet("claim 2", "quote 2", source_id="s1", source_quality=0.6))
    ledger.add(_packet("claim 3", "quote 3", source_id="s2", source_quality=0.2))

    assert ledger.source_quality("s1") == pytest.approx(0.8)
    assert ledger.source_quality("s2") == pytest.approx(0.2)
    assert ledger.source_quality("unknown-source") is None
