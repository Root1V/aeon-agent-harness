"""EVAL-002's acceptance test: running the real `deep_research_core` suite (EVAL-001,
evals/suites/deep_research_core.yaml) offline produces a real report — both graders actually
exercise DR-001..DR-005's pipeline code, not a stand-in.
"""
from __future__ import annotations

import pytest

from aeon_evalops.runner import CASE_RUNNERS, GRADER_RUNNERS, EvalSuiteNotFoundError, format_report, run_suite


async def test_eval_run_produces_a_report_for_deep_research_core():
    report = await run_suite("deep_research_core", trials=2)

    assert report.suite_name == "deep_research_core"
    assert report.trials == 2
    assert {g.name for g in report.graders} == {"coverage_grader", "citation_integrity_grader"}
    assert report.passed is True
    for g in report.graders:
        assert g.status == "PASS"
        assert g.score == 1.0
        assert g.threshold is not None


async def test_eval_run_report_is_human_readable():
    report = await run_suite("deep_research_core", trials=1)
    text = format_report(report)

    assert "deep_research_core" in text
    assert "coverage_grader" in text
    assert "citation_integrity_grader" in text
    assert "PASS" in text


async def test_eval_run_skips_all_graders_for_a_suite_with_no_registered_case_runner():
    """injection_suite is a real, valid EvalSuite (EVAL-001) but has no offline harness yet — the
    Runner must say so honestly rather than reporting a fabricated PASS/FAIL."""
    report = await run_suite("injection_suite", trials=1)

    assert report.graders
    for g in report.graders:
        assert g.status == "SKIPPED"
        assert g.score is None
    # An all-skipped suite must never read as a plain PASS — nothing was actually verified.
    assert report.status == "SKIPPED"
    assert "SKIPPED" in format_report(report)


async def test_eval_run_rejects_an_unknown_suite_name():
    with pytest.raises(EvalSuiteNotFoundError):
        await run_suite("not-a-real-suite", trials=1)


async def test_eval_run_fails_a_grader_that_scores_below_its_threshold(monkeypatch):
    async def always_insufficient(_case: dict) -> tuple[bool, bool]:
        return False, False

    monkeypatch.setitem(CASE_RUNNERS, "deep_research_core", always_insufficient)

    report = await run_suite("deep_research_core", trials=1)

    assert report.passed is False
    for g in report.graders:
        assert g.status == "FAIL"
        assert g.score == 0.0


def test_grader_runners_average_across_multiple_case_outcomes():
    outcomes = [(True, True), (True, False), (False, True), (False, False)]
    assert GRADER_RUNNERS["coverage_grader"](outcomes) == pytest.approx(0.5)
    assert GRADER_RUNNERS["citation_integrity_grader"](outcomes) == pytest.approx(0.5)
