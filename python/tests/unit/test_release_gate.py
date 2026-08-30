"""EVAL-003's acceptance test: a candidate whose eval results regress against the currently
Released baseline is blocked from promotion — even when it still clears its own suite's static
thresholds (test_release_gate_blocks_regression).
"""
from __future__ import annotations

from aeon_evalops.release_gate import evaluate_release_gate
from aeon_evalops.runner import GraderResult, SuiteReport


def _report(*graders: GraderResult, trials: int = 1) -> SuiteReport:
    return SuiteReport(suite_name="deep_research_core", trials=trials, graders=list(graders))


def test_release_gate_blocks_regression_even_when_still_above_threshold():
    """The core property: candidate score dropped from baseline (1.0 -> 0.85) but is still above
    the suite's own PASS threshold (0.8) — EVAL-002 alone would call this PASS, but it's a real
    regression and the gate must block it."""
    baseline = _report(GraderResult("coverage_grader", "PASS", 1.0, 0.8))
    candidate = _report(GraderResult("coverage_grader", "PASS", 0.85, 0.8))

    decision = evaluate_release_gate(baseline, candidate)

    assert decision.allowed is False
    assert len(decision.regressions) == 1
    assert decision.regressions[0].grader_name == "coverage_grader"
    assert decision.regressions[0].baseline_score == 1.0
    assert decision.regressions[0].candidate_score == 0.85
    assert "regressed" in decision.reason


def test_release_gate_allows_a_candidate_that_matches_or_improves_on_baseline():
    baseline = _report(GraderResult("coverage_grader", "PASS", 0.9, 0.8))
    candidate = _report(GraderResult("coverage_grader", "PASS", 0.95, 0.8))

    decision = evaluate_release_gate(baseline, candidate)

    assert decision.allowed is True
    assert decision.regressions == []
    assert decision.failing_graders == []


def test_release_gate_blocks_on_an_outright_failing_grader_even_without_a_baseline_regression():
    baseline = _report(GraderResult("citation_integrity_grader", "PASS", 0.6, 0.5))
    candidate = _report(GraderResult("citation_integrity_grader", "FAIL", 0.4, 0.5))

    decision = evaluate_release_gate(baseline, candidate)

    assert decision.allowed is False
    assert decision.failing_graders == ["citation_integrity_grader"]


def test_release_gate_ignores_a_grader_skipped_on_either_side():
    baseline = _report(GraderResult("injection_resistance_grader", "SKIPPED", None, 1.0, "no harness yet"))
    candidate = _report(GraderResult("injection_resistance_grader", "SKIPPED", None, 1.0, "no harness yet"))

    decision = evaluate_release_gate(baseline, candidate)

    assert decision.allowed is True
    assert decision.regressions == []


def test_release_gate_ignores_a_grader_with_no_baseline_counterpart():
    """A brand-new grader added to a suite has nothing to regress against yet — it shouldn't block
    the very first candidate that introduces it."""
    baseline = _report(GraderResult("coverage_grader", "PASS", 0.9, 0.8))
    candidate = _report(
        GraderResult("coverage_grader", "PASS", 0.9, 0.8),
        GraderResult("new_grader", "PASS", 0.7, 0.5),
    )

    decision = evaluate_release_gate(baseline, candidate)

    assert decision.allowed is True


def test_release_gate_respects_a_configured_regression_tolerance():
    baseline = _report(GraderResult("coverage_grader", "PASS", 1.0, 0.8))
    candidate = _report(GraderResult("coverage_grader", "PASS", 0.97, 0.8))

    strict = evaluate_release_gate(baseline, candidate, regression_tolerance=0.0)
    lenient = evaluate_release_gate(baseline, candidate, regression_tolerance=0.05)

    assert strict.allowed is False
    assert lenient.allowed is True


def test_release_gate_multiple_regressions_are_all_reported():
    baseline = _report(
        GraderResult("coverage_grader", "PASS", 1.0, 0.8),
        GraderResult("citation_integrity_grader", "PASS", 1.0, 0.95),
    )
    candidate = _report(
        GraderResult("coverage_grader", "PASS", 0.85, 0.8),
        GraderResult("citation_integrity_grader", "PASS", 1.0, 0.95),
    )

    decision = evaluate_release_gate(baseline, candidate)

    assert decision.allowed is False
    assert {v.grader_name for v in decision.regressions} == {"coverage_grader"}
