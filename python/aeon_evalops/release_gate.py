"""Release Gate (EVAL-003): blocks an agent's promotion from Candidate to Released when its eval
results regress against the currently Released version's own baseline — even when the candidate's
results still clear each grader's static threshold (EVAL-002's own PASS/FAIL). "Still passes" and
"didn't get worse than what's already in production" are different promises; a Release Gate
enforces the second one, not just the first.

The gate never re-runs a suite or recomputes a grade — it only compares two SuiteReports (aeon_
evalops.runner.run_suite's output) a caller already has: the currently Released version's baseline,
and the candidate's fresh result. The Go Agent Registry (go/internal/store/agent_registry.go's
TransitionLifecycle) applies exactly this module's verdict — computed here, never on the Go side,
which has no way to run an eval suite.
"""
from __future__ import annotations

from dataclasses import dataclass, field

from aeon_evalops.runner import SuiteReport


@dataclass
class RegressionViolation:
    grader_name: str
    baseline_score: float
    candidate_score: float


@dataclass
class ReleaseGateDecision:
    allowed: bool
    regressions: list[RegressionViolation] = field(default_factory=list)
    failing_graders: list[str] = field(default_factory=list)  # candidate graders whose own status is FAIL

    @property
    def reason(self) -> str:
        if self.allowed:
            return "release gate passed: no regression against baseline, no failing grader"
        parts = []
        if self.failing_graders:
            parts.append(f"failing grader(s): {', '.join(self.failing_graders)}")
        for v in self.regressions:
            parts.append(f"{v.grader_name} regressed from {v.baseline_score:.3f} to {v.candidate_score:.3f}")
        return "; ".join(parts)


def evaluate_release_gate(
    baseline: SuiteReport, candidate: SuiteReport, regression_tolerance: float = 0.0
) -> ReleaseGateDecision:
    """A candidate may promote only if (a) none of its own graders report status FAIL (EVAL-002's
    absolute threshold), and (b) no grader's score dropped by more than `regression_tolerance`
    relative to the SAME-named grader's score in `baseline`. A grader with no baseline counterpart,
    or a SKIPPED grader on either side, is neither a pass nor a regression — there is nothing real
    to compare, so it never blocks or unblocks a promotion by itself."""
    failing_graders = [g.name for g in candidate.graders if g.status == "FAIL"]

    baseline_by_name = {g.name: g for g in baseline.graders}
    regressions: list[RegressionViolation] = []
    for g in candidate.graders:
        if g.status == "SKIPPED" or g.score is None:
            continue
        baseline_grader = baseline_by_name.get(g.name)
        if baseline_grader is None or baseline_grader.status == "SKIPPED" or baseline_grader.score is None:
            continue
        if g.score < baseline_grader.score - regression_tolerance:
            regressions.append(RegressionViolation(g.name, baseline_grader.score, g.score))

    return ReleaseGateDecision(
        allowed=not failing_graders and not regressions,
        regressions=regressions,
        failing_graders=failing_graders,
    )
