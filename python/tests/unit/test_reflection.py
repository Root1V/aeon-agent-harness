"""MEM-003's acceptance test: Reflection extracts real memory candidates from a completed run
(test_reflection_extracts_candidates), grounded only in evidence the run actually produced — same
grounding discipline as DR-004's Reporter/DR-005's Citation Verifier, applied to memory instead of
citations.
"""
from __future__ import annotations

import json

import pytest

from aeon_memory.reflection import (
    Reflector,
    ReflectionError,
    RunSummary,
    build_reflection_request,
    parse_reflection,
)


def _response(content: dict | str) -> dict:
    text = content if isinstance(content, str) else json.dumps(content)
    return {
        "model": "test-model",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
    }


def _run(evidence_refs: list[str] | None = None) -> RunSummary:
    return RunSummary(
        run_id="run-123",
        query="what is the capital of France?",
        outcome="success",
        report_text="Paris is the capital of France.",
        evidence_refs=evidence_refs or ["claim-1"],
    )


async def test_reflection_extracts_candidates():
    run_summary = _run(["claim-1"])

    async def fake_decide(rendered_context: dict) -> dict:
        return _response(
            {
                "candidates": [
                    {
                        "type": "SEMANTIC",
                        "scope": "project",
                        "content": "The capital of France is Paris.",
                        "evidence_refs": ["claim-1"],
                        "confidence": 0.9,
                    }
                ]
            }
        )

    candidates = await Reflector(model="test-model").reflect(run_summary, fake_decide)

    assert len(candidates) == 1
    candidate = candidates[0]
    assert candidate.type == "SEMANTIC"
    assert candidate.scope == "project"
    assert candidate.content == "The capital of France is Paris."
    assert candidate.evidence_refs == ["claim-1"]
    assert candidate.source_run_ids == ["run-123"]
    assert candidate.confidence == 0.9


async def test_reflection_can_extract_zero_candidates_when_nothing_is_worth_remembering():
    run_summary = _run([])

    async def fake_decide(rendered_context: dict) -> dict:
        return _response({"candidates": []})

    candidates = await Reflector(model="test-model").reflect(run_summary, fake_decide)
    assert candidates == []


def test_reflection_request_is_tool_less():
    request = build_reflection_request(_run(), "test-model")
    assert "tools" not in request
    assert "tool_choice" not in request
    assert set(request) == {"model", "messages"}


def test_reflection_rejects_a_candidate_citing_an_evidence_ref_the_run_never_produced():
    run_summary = _run(["claim-1"])
    raw_output = _response(
        {
            "candidates": [
                {
                    "type": "SEMANTIC",
                    "scope": "project",
                    "content": "invented fact",
                    "evidence_refs": ["claim-999-never-existed"],
                    "confidence": 0.9,
                }
            ]
        }
    )

    with pytest.raises(ReflectionError):
        parse_reflection(raw_output, run_summary)


@pytest.mark.parametrize("bad_type", ["NOT_A_TYPE", "", None])
def test_reflection_rejects_an_invalid_type(bad_type):
    raw_output = _response(
        {"candidates": [{"type": bad_type, "scope": "project", "content": "x", "evidence_refs": [], "confidence": 0.5}]}
    )
    with pytest.raises(ReflectionError):
        parse_reflection(raw_output, _run())


@pytest.mark.parametrize("bad_scope", ["not-a-scope", "", None])
def test_reflection_rejects_an_invalid_scope(bad_scope):
    raw_output = _response(
        {"candidates": [{"type": "SEMANTIC", "scope": bad_scope, "content": "x", "evidence_refs": [], "confidence": 0.5}]}
    )
    with pytest.raises(ReflectionError):
        parse_reflection(raw_output, _run())


@pytest.mark.parametrize("bad_confidence", [-0.1, 1.1, "high"])
def test_reflection_rejects_an_invalid_confidence(bad_confidence):
    raw_output = _response(
        {
            "candidates": [
                {"type": "SEMANTIC", "scope": "project", "content": "x", "evidence_refs": [], "confidence": bad_confidence}
            ]
        }
    )
    with pytest.raises(ReflectionError):
        parse_reflection(raw_output, _run())


def test_reflection_rejects_missing_candidates_key():
    with pytest.raises(ReflectionError):
        parse_reflection(_response({"not_candidates": []}), _run())


def test_reflection_rejects_non_json_output():
    with pytest.raises(ReflectionError):
        parse_reflection(_response("just prose, not JSON"), _run())


def test_reflection_rejects_empty_content():
    raw_output = _response(
        {"candidates": [{"type": "SEMANTIC", "scope": "project", "content": "", "evidence_refs": [], "confidence": 0.5}]}
    )
    with pytest.raises(ReflectionError):
        parse_reflection(raw_output, _run())
