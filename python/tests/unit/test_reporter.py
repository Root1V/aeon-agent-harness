"""DR-004's acceptance test: the Reporter's model request never offers a tools/tool_choice field —
structurally tool-less, not just instructed not to call tools (test_reporter_no_tools_available) —
and it never lets a report through that cites a claim outside the allowed set.
"""
from __future__ import annotations

import json
import uuid

import pytest

from aeon_evidence.ledger import EvidenceLedger
from aeon_profiles.deep_research.reporter import (
    Reporter,
    ReporterError,
    build_reporter_request,
    select_allowed_claims,
)
from aeon_profiles.deep_research.sufficiency_gate import CoverageEntry, SufficiencyDecision


def _claim(claim_id: str, text: str = "a claim", source_id: str = "src-1") -> dict:
    return {
        "claim_id": claim_id,
        "subtopic_id": "st-A",
        "claim": text,
        "quote": f'"{text}"',
        "source_id": source_id,
        "retrieved_at": "2026-08-29T00:00:00+00:00",
        "source_quality": 0.8,
        "confidence": 0.8,
        "support": "SUPPORTS",
    }


def _response(content: dict | str) -> dict:
    text = content if isinstance(content, str) else json.dumps(content)
    return {
        "model": "test-model",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
    }


def test_reporter_no_tools_available():
    claims = [_claim("claim-1")]
    request = build_reporter_request("what happened?", claims, "test-model")

    assert "tools" not in request
    assert "tool_choice" not in request
    assert set(request) == {"model", "messages"}


async def test_reporter_no_tools_available_end_to_end_through_report():
    """The same guarantee holds for the actual request Reporter.report sends to `decide`, not just
    build_reporter_request in isolation."""
    seen_requests = []
    claim_id = str(uuid.uuid4())

    async def fake_decide(rendered_context: dict) -> dict:
        seen_requests.append(rendered_context)
        return _response({"text": "summary", "cited_claim_ids": [claim_id]})

    claims = [_claim(claim_id)]
    await Reporter(model="test-model").report("q", claims, fake_decide)

    assert len(seen_requests) == 1
    assert "tools" not in seen_requests[0]


async def test_reporter_accepts_a_report_that_only_cites_allowed_claims():
    claim_id = str(uuid.uuid4())

    async def fake_decide(rendered_context: dict) -> dict:
        return _response({"text": "the sky is blue [%s]" % claim_id, "cited_claim_ids": [claim_id]})

    draft = await Reporter(model="test-model").report("q", [_claim(claim_id)], fake_decide)

    assert draft.cited_claim_ids == [claim_id]
    assert draft.query == "q"


async def test_reporter_rejects_a_report_that_cites_an_unlisted_claim_id():
    allowed_id = str(uuid.uuid4())
    invented_id = str(uuid.uuid4())

    async def fake_decide(rendered_context: dict) -> dict:
        return _response({"text": "made up fact", "cited_claim_ids": [invented_id]})

    with pytest.raises(ReporterError):
        await Reporter(model="test-model").report("q", [_claim(allowed_id)], fake_decide)


async def test_reporter_rejects_a_report_mixing_allowed_and_unlisted_claims():
    allowed_id = str(uuid.uuid4())
    invented_id = str(uuid.uuid4())

    async def fake_decide(rendered_context: dict) -> dict:
        return _response({"text": "partly real, partly invented", "cited_claim_ids": [allowed_id, invented_id]})

    with pytest.raises(ReporterError):
        await Reporter(model="test-model").report("q", [_claim(allowed_id)], fake_decide)


async def test_reporter_rejects_output_missing_cited_claim_ids():
    async def fake_decide(rendered_context: dict) -> dict:
        return _response({"text": "no citations field at all"})

    with pytest.raises(ReporterError):
        await Reporter(model="test-model").report("q", [_claim("claim-1")], fake_decide)


async def test_reporter_rejects_non_json_output():
    async def fake_decide(rendered_context: dict) -> dict:
        return _response("just prose, not JSON")

    with pytest.raises(ReporterError):
        await Reporter(model="test-model").report("q", [_claim("claim-1")], fake_decide)


def test_select_allowed_claims_excludes_contested_and_incomplete_subtasks():
    ledger = EvidenceLedger()
    covered = ledger.add(_claim("c-covered", source_id="src-covered", text="covered claim") | {"subtopic_id": "st-covered"})
    contested_a = ledger.add(_claim("c-contested-a", source_id="src-a", text="contested a") | {"subtopic_id": "st-contested"})
    ledger.add(
        _claim("c-contested-b", source_id="src-b", text="contested b")
        | {"subtopic_id": "st-contested", "support": "CONTRADICTS"},
        contradicts=[contested_a["claim_id"]],
    )
    ledger.add(_claim("c-incomplete", source_id="src-incomplete", text="incomplete") | {"subtopic_id": "st-incomplete"})

    decision = SufficiencyDecision(
        sufficient=False,
        coverage=[
            CoverageEntry("st-covered", "topic-covered", covered=True, contested=False, reason="sufficient"),
            CoverageEntry("st-contested", "topic-contested", covered=True, contested=True, reason="unresolved_contradiction"),
            CoverageEntry("st-incomplete", "topic-incomplete", covered=False, contested=False, reason="no_supporting_evidence"),
        ],
        topics_to_replan=["topic-contested", "topic-incomplete"],
    )

    allowed = select_allowed_claims(decision, ledger)

    assert [c["claim_id"] for c in allowed] == [covered["claim_id"]]
