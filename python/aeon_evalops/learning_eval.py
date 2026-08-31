"""Learning Eval (EVAL-004): forward/negative transfer, usefulness, staleness — the VALIDATE step
`MEM-002`'s candidate pipeline needs a real signal for, instead of `ValidationDecision`/
`PromotionDecision` only ever being constructed by hand in tests (see backlog.md's prior note on
this — this module is what closes that gap).

- **Forward transfer**: does injecting a memory candidate into context improve a *related* probe's
  answer, versus a scripted baseline without it? A candidate that never demonstrably helps anything
  has no business being promoted.
- **Negative transfer**: does injecting it change an *unrelated/conflicting* probe's answer? A
  memory that leaks into contexts it has nothing to do with — and changes the outcome there — is
  actively harmful, not just unhelpful.
- **Usefulness/staleness**: MEM-005's own `DecayedUtility` projection, supplied by the caller (this
  module never talks to Postgres) — a candidate whose usage-decayed utility has already fallen
  below a floor is stale regardless of how well it does on a probe.

Every probe here is scripted/offline (no live LLM, no cost) — same property as
`aeon_evalops.runner`'s other real suites, and the same `decide` injection pattern as
`aeon_profiles.deep_research`'s modules (docs/adr/0001): this module has no Temporal import and no
network access of its own.
"""
from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any

DecideFn = Callable[[dict[str, Any]], Awaitable[dict[str, Any]]]


@dataclass
class MemoryCandidateUnderTest:
    content: str
    utility_score: float
    decayed_utility: float  # MEM-005's DecayedUtility, computed by the caller at eval time
    staleness_floor: float = 0.2


@dataclass
class TransferProbe:
    probe_id: str
    kind: str  # "related" | "conflicting"
    query: str
    answer_without_memory: str
    answer_with_memory: str  # related: the improved answer memory should produce.
    # conflicting: must equal answer_without_memory — memory must not change this answer at all.


@dataclass
class ProbeResult:
    probe_id: str
    kind: str
    baseline_answer: str
    with_memory_answer: str
    improved: bool  # only meaningful for "related"
    unchanged: bool  # only meaningful for "conflicting"


@dataclass
class LearningEvalReport:
    candidate: MemoryCandidateUnderTest
    probe_results: list[ProbeResult] = field(default_factory=list)
    is_stale: bool = False

    @property
    def forward_transfer_ok(self) -> bool:
        """True only if every related probe actually improved AND the candidate isn't stale — a
        stale candidate gets no credit for a probe result computed as if it were fresh."""
        related = [p for p in self.probe_results if p.kind == "related"]
        if self.is_stale:
            return False
        return all(p.improved for p in related) if related else True

    @property
    def negative_transfer_ok(self) -> bool:
        conflicting = [p for p in self.probe_results if p.kind == "conflicting"]
        return all(p.unchanged for p in conflicting) if conflicting else True


def _extract_text(raw_model_output: dict[str, Any]) -> str:
    return raw_model_output["choices"][0]["message"]["content"]


async def run_transfer_probe(probe: TransferProbe, candidate: MemoryCandidateUnderTest, decide: DecideFn) -> ProbeResult:
    """Runs the same query twice — once with no memory in context, once with the candidate's
    content injected as a system message — and compares the two answers against the probe's own
    scripted expectations."""
    baseline_raw = await decide({"messages": [{"role": "user", "content": probe.query}]})
    baseline_answer = _extract_text(baseline_raw)

    with_memory_raw = await decide(
        {
            "messages": [
                {"role": "system", "content": f"Known fact: {candidate.content}"},
                {"role": "user", "content": probe.query},
            ]
        }
    )
    with_memory_answer = _extract_text(with_memory_raw)

    return ProbeResult(
        probe_id=probe.probe_id,
        kind=probe.kind,
        baseline_answer=baseline_answer,
        with_memory_answer=with_memory_answer,
        improved=(probe.kind == "related" and with_memory_answer == probe.answer_with_memory),
        unchanged=(probe.kind == "conflicting" and with_memory_answer == probe.answer_without_memory),
    )


async def evaluate_learning(
    candidate: MemoryCandidateUnderTest, probes: list[TransferProbe], decide: DecideFn
) -> LearningEvalReport:
    results = [await run_transfer_probe(p, candidate, decide) for p in probes]
    return LearningEvalReport(
        candidate=candidate, probe_results=results, is_stale=candidate.decayed_utility < candidate.staleness_floor
    )


@dataclass
class ValidationDecision:
    """Shaped to match go/internal/store's ValidationDecision (MEM-002) field-for-field, so a
    caller can pass this straight through to `POST /memory/{id}/validate`."""

    allowed: bool
    reason: str = ""


@dataclass
class PromotionDecision:
    """Shaped to match go/internal/store's PromotionDecision (MEM-002) field-for-field."""

    allowed: bool
    reason: str = ""


def compute_validation_decision(report: LearningEvalReport) -> ValidationDecision:
    """A candidate only clears QUARANTINED -> VALIDATED if it demonstrates real forward transfer,
    shows no negative transfer, and isn't already stale. Checked in this order so the reason
    surfaced is always the most fundamental problem, not an incidental one."""
    if report.is_stale:
        return ValidationDecision(allowed=False, reason="candidate's decayed utility is below the staleness floor")
    if not report.negative_transfer_ok:
        return ValidationDecision(
            allowed=False, reason="candidate changed the answer to an unrelated/conflicting probe (negative transfer)"
        )
    if not report.forward_transfer_ok:
        return ValidationDecision(
            allowed=False, reason="candidate did not improve any related probe (no demonstrated forward transfer)"
        )
    return ValidationDecision(allowed=True)


def compute_promotion_decision(report: LearningEvalReport) -> PromotionDecision:
    """Promotion uses the same bar as validation today. A stricter bar for VALIDATED -> ACTIVE
    specifically (e.g. a minimum trial count, or requiring human sign-off) is real future work —
    see backlog.md — not invented here."""
    decision = compute_validation_decision(report)
    return PromotionDecision(allowed=decision.allowed, reason=decision.reason)
