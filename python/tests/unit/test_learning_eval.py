"""EVAL-004's acceptance test lives in test_eval_runner.py (the real `learning_eval` suite report);
this file tests the pure logic directly — forward/negative transfer, staleness, and the
ValidationDecision/PromotionDecision this module computes (closing the gap backlog.md noted:
MEM-002 could only ever apply these decisions, never compute them).
"""
from __future__ import annotations

from aeon_evalops.learning_eval import (
    LearningEvalReport,
    MemoryCandidateUnderTest,
    TransferProbe,
    compute_promotion_decision,
    compute_validation_decision,
    evaluate_learning,
)


def _response(text: str) -> dict:
    return {
        "model": "test-model",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
    }


def _fresh_candidate(content: str = "The capital of France is Paris.") -> MemoryCandidateUnderTest:
    return MemoryCandidateUnderTest(content=content, utility_score=1.0, decayed_utility=0.9, staleness_floor=0.2)


async def test_learning_eval_detects_forward_transfer():
    candidate = _fresh_candidate()
    probe = TransferProbe(
        probe_id="p1", kind="related", query="What is the capital of France?",
        answer_without_memory="unknown", answer_with_memory="Paris",
    )

    async def scripted_decide(rendered_context: dict) -> dict:
        has_memory = any("Known fact:" in m["content"] for m in rendered_context["messages"] if m["role"] == "system")
        return _response("Paris" if has_memory else "unknown")

    report = await evaluate_learning(candidate, [probe], scripted_decide)

    assert report.forward_transfer_ok is True
    assert report.negative_transfer_ok is True  # no conflicting probes at all — vacuously fine


async def test_learning_eval_forward_transfer_fails_when_memory_does_not_help():
    candidate = _fresh_candidate()
    probe = TransferProbe(
        probe_id="p1", kind="related", query="What is the capital of France?",
        answer_without_memory="unknown", answer_with_memory="Paris",
    )

    async def scripted_decide(_rendered_context: dict) -> dict:
        return _response("unknown")  # memory made no difference at all

    report = await evaluate_learning(candidate, [probe], scripted_decide)
    assert report.forward_transfer_ok is False


async def test_learning_eval_detects_negative_transfer():
    candidate = _fresh_candidate()
    probe = TransferProbe(
        probe_id="p1", kind="conflicting", query="What is the capital of Germany?",
        answer_without_memory="Berlin", answer_with_memory="Berlin",
    )

    async def scripted_decide(rendered_context: dict) -> dict:
        has_memory = any("Known fact:" in m["content"] for m in rendered_context["messages"] if m["role"] == "system")
        # A misbehaving candidate leaks into an unrelated query and changes the answer.
        return _response("Paris" if has_memory else "Berlin")

    report = await evaluate_learning(candidate, [probe], scripted_decide)

    assert report.negative_transfer_ok is False
    assert report.forward_transfer_ok is True  # no related probes — vacuously fine


async def test_learning_eval_no_negative_transfer_when_answer_is_unchanged():
    candidate = _fresh_candidate()
    probe = TransferProbe(
        probe_id="p1", kind="conflicting", query="What is the capital of Germany?",
        answer_without_memory="Berlin", answer_with_memory="Berlin",
    )

    async def scripted_decide(_rendered_context: dict) -> dict:
        return _response("Berlin")  # unaffected by the injected memory either way

    report = await evaluate_learning(candidate, [probe], scripted_decide)
    assert report.negative_transfer_ok is True


async def test_learning_eval_flags_staleness_and_forces_forward_transfer_false():
    stale_candidate = MemoryCandidateUnderTest(
        content="an old fact", utility_score=0.1, decayed_utility=0.05, staleness_floor=0.2
    )
    probe = TransferProbe(
        probe_id="p1", kind="related", query="q", answer_without_memory="unknown", answer_with_memory="right answer"
    )

    async def scripted_decide(rendered_context: dict) -> dict:
        has_memory = any("Known fact:" in m["content"] for m in rendered_context["messages"] if m["role"] == "system")
        return _response("right answer" if has_memory else "unknown")

    report = await evaluate_learning(stale_candidate, [probe], scripted_decide)

    assert report.is_stale is True
    # Even though the probe itself would have "improved", staleness must override that.
    assert report.forward_transfer_ok is False


async def test_compute_validation_decision_allows_a_clean_candidate():
    candidate = _fresh_candidate()
    probe = TransferProbe(
        probe_id="p1", kind="related", query="q", answer_without_memory="unknown", answer_with_memory="right"
    )

    async def scripted_decide(rendered_context: dict) -> dict:
        has_memory = any("Known fact:" in m["content"] for m in rendered_context["messages"] if m["role"] == "system")
        return _response("right" if has_memory else "unknown")

    report = await evaluate_learning(candidate, [probe], scripted_decide)
    decision = compute_validation_decision(report)

    assert decision.allowed is True
    assert decision.reason == ""


async def test_compute_validation_decision_blocks_on_staleness():
    async def unreachable_decide(_rendered_context: dict) -> dict:
        raise AssertionError("no probes were given — decide should never be called")

    candidate = MemoryCandidateUnderTest(content="x", utility_score=0.0, decayed_utility=0.01, staleness_floor=0.2)
    report = await evaluate_learning(candidate, [], unreachable_decide)  # no probes needed to prove staleness blocks

    decision = compute_validation_decision(report)
    assert decision.allowed is False
    assert "stale" in decision.reason


async def test_compute_validation_decision_blocks_on_negative_transfer():
    candidate = _fresh_candidate()
    probe = TransferProbe(
        probe_id="p1", kind="conflicting", query="q", answer_without_memory="Berlin", answer_with_memory="Berlin"
    )

    async def scripted_decide(rendered_context: dict) -> dict:
        has_memory = any("Known fact:" in m["content"] for m in rendered_context["messages"] if m["role"] == "system")
        return _response("Paris" if has_memory else "Berlin")

    report = await evaluate_learning(candidate, [probe], scripted_decide)
    decision = compute_validation_decision(report)

    assert decision.allowed is False
    assert "negative transfer" in decision.reason


def test_compute_promotion_decision_mirrors_validation_shape():
    # No probes, no staleness: both decisions should agree and be allowed.
    report = LearningEvalReport(candidate=_fresh_candidate(), probe_results=[], is_stale=False)
    validation = compute_validation_decision(report)
    promotion = compute_promotion_decision(report)

    assert promotion.allowed == validation.allowed
    assert promotion.reason == validation.reason
