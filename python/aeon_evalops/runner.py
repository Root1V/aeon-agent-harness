"""Eval Runner (EVAL-002): runs a registered EvalSuite (EVAL-001, evals/suites/*.yaml) offline —
every model/tool call in a run is a deterministic script keyed off the dataset case, never a live
provider call — for a configurable number of repeated trials, and produces a per-grader report
scored against the suite's own thresholds.

"Offline" and "repeated trials" are real properties of this Runner: nothing here costs money or
depends on network access, and trials are reproducible (useful once a real source of nondeterminism
— an actual provider call — replaces today's scripted fixture, which is a distinct future increment,
not this one). "Provider matrix" and "trace graders" are out of scope for this first Runner — see
backlog.md. What IS real today: `coverage_grader` and `citation_integrity_grader` call the actual
DR-001..DR-005 pipeline code (Planner, Researcher, Sufficiency Gate, Reporter, Citation Verifier),
not a stand-in — a regression in any of those modules shows up here as a real grade drop.

A suite named in evals/suites/ with no registered case runner (nothing here can synthesize a
meaningful offline exercise of it yet, e.g. injection_suite) reports every one of its graders as
SKIPPED rather than fabricating a score — see CASE_RUNNERS below.
"""
from __future__ import annotations

import json
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml
from jsonschema import Draft202012Validator

from aeon_evalops.learning_eval import MemoryCandidateUnderTest, TransferProbe, evaluate_learning
from aeon_evidence.ledger import EvidenceLedger
from aeon_profiles.deep_research.citation_verifier import verify_and_repair
from aeon_profiles.deep_research.planner import MAX_SUBTASKS, MIN_SUBTASKS, Planner
from aeon_profiles.deep_research.reporter import Reporter, select_allowed_claims
from aeon_profiles.deep_research.researcher import run_researchers_in_parallel
from aeon_profiles.deep_research.sufficiency_gate import evaluate_sufficiency

REPO_ROOT = Path(__file__).resolve().parents[2]


class EvalSuiteNotFoundError(FileNotFoundError):
    """Raised when `run_suite`/`load_suite` is asked for a suite with no matching file under
    evals/suites/ — a typo'd suite name must fail loudly, not silently run zero graders."""


@dataclass
class GraderResult:
    name: str
    status: str  # "PASS" | "FAIL" | "SKIPPED"
    score: float | None
    threshold: float | None
    detail: str = ""


@dataclass
class SuiteReport:
    suite_name: str
    trials: int
    graders: list[GraderResult] = field(default_factory=list)

    @property
    def status(self) -> str:
        """"FAIL" if any grader failed; "SKIPPED" if every grader was skipped (nothing was actually
        verified — must never be reported as a plain PASS, or a suite with no real harness yet
        would look identical to one that genuinely ran and succeeded); "PASS" otherwise."""
        if any(g.status == "FAIL" for g in self.graders):
            return "FAIL"
        if self.graders and all(g.status == "SKIPPED" for g in self.graders):
            return "SKIPPED"
        return "PASS"

    @property
    def passed(self) -> bool:
        return self.status != "FAIL"


def load_suite(suite_name: str) -> dict[str, Any]:
    path = REPO_ROOT / "evals" / "suites" / f"{suite_name}.yaml"
    if not path.exists():
        raise EvalSuiteNotFoundError(f"no such eval suite: {suite_name!r} (expected {path})")

    doc = yaml.safe_load(path.read_text())
    schema = json.loads((REPO_ROOT / "proto" / "manifests" / "eval_suite.schema.json").read_text())
    Draft202012Validator(schema).validate(doc)
    return doc


def load_dataset(dataset_relpath: str) -> list[dict[str, Any]]:
    path = REPO_ROOT / dataset_relpath
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def _normalized(content: dict[str, Any]) -> dict[str, Any]:
    return {
        "model": "eval-fixture-model",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": json.dumps(content)}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
    }


def _normalized_text(text: str) -> dict[str, Any]:
    """Same NormalizedChatResponse envelope as _normalized, but for a plain-text answer (learning_eval's
    probes compare answers directly, not JSON-decoded content)."""
    return {
        "model": "eval-fixture-model",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
    }


async def _run_deep_research_core_case(case: dict[str, Any]) -> tuple[bool, bool]:
    """Runs the real DR-001..DR-005 pipeline for one dataset case against scripted, offline
    decide/execute_tool fixtures — every subtask's Researcher calls one tool, gets a canned but
    genuinely SUPPORTS-quality result, and finishes; the Reporter cites exactly what the Sufficiency
    Gate allowed. Returns (sufficiency_ok, citation_ok)."""
    topics = case["expected_topics"]
    if not MIN_SUBTASKS <= len(topics) <= MAX_SUBTASKS:
        raise ValueError(f"case {case.get('id')!r}: expected_topics must have {MIN_SUBTASKS}-{MAX_SUBTASKS} entries, got {len(topics)}")

    async def planner_decide(_rendered_context: dict[str, Any]) -> dict[str, Any]:
        subtasks = [
            {
                "id": f"st-{i}",
                "description": f"Investigate {topic}",
                "coverage_topic": topic,
                "budget": {"max_tool_calls": 2, "max_model_calls": 2},
            }
            for i, topic in enumerate(topics)
        ]
        return _normalized({"query": case["query"], "subtasks": subtasks})

    plan = await Planner(model="eval-fixture-model").plan(case["query"], planner_decide)

    async def researcher_decide(rendered_context: dict[str, Any]) -> dict[str, Any]:
        if len(rendered_context["messages"]) <= 2:
            topic = rendered_context["messages"][1]["content"]
            return _normalized({"action": "CALL_TOOL", "tool_name": "search.web", "args": {"query": topic}})
        return _normalized({"action": "FINISH", "message": "done"})

    async def execute_tool(_tool_name: str, args: dict[str, Any]) -> dict[str, Any]:
        return {"status": "ok", "content": f"a real-looking fact about {args.get('query', '')}"}

    results = await run_researchers_in_parallel(plan, "eval-fixture-model", researcher_decide, execute_tool)

    ledger = EvidenceLedger()
    for subtask, result in zip(plan.subtasks, results, strict=True):
        for i, call in enumerate(result.tool_calls):
            ledger.add(
                {
                    "claim_id": f"{subtask.id}-claim-{i}",
                    "subtopic_id": subtask.id,
                    "claim": call.result["content"],
                    "quote": call.result["content"],
                    "source_id": f"{subtask.id}-source-{i}",
                    "retrieved_at": "2026-08-30T00:00:00+00:00",
                    "source_quality": 0.8,
                    "confidence": 0.8,
                    "support": "SUPPORTS",
                }
            )

    decision = evaluate_sufficiency(plan, results, ledger)
    allowed_claims = select_allowed_claims(decision, ledger)

    async def reporter_decide(_rendered_context: dict[str, Any]) -> dict[str, Any]:
        return _normalized(
            {"text": " ".join(c["claim"] for c in allowed_claims), "cited_claim_ids": [c["claim_id"] for c in allowed_claims]}
        )

    draft = await Reporter(model="eval-fixture-model").report(case["query"], allowed_claims, reporter_decide)
    verification = verify_and_repair(draft, decision, ledger)

    return decision.sufficient, verification.verified


async def _run_learning_eval_case(case: dict[str, Any]) -> tuple[bool, bool]:
    """Runs the real aeon_evalops.learning_eval logic (EVAL-004) for one dataset case — a memory
    candidate plus a set of scripted transfer probes — against a fully offline, deterministic
    decide() built straight from the case's own fields (same technique as
    _run_deep_research_core_case's planner_decide/researcher_decide closures). Returns
    (forward_transfer_ok, negative_transfer_ok), the same (bool, bool) shape every other case
    runner here produces, so GRADER_RUNNERS needs no special-casing for this suite."""
    candidate = MemoryCandidateUnderTest(
        content=case["candidate"]["content"],
        utility_score=case["candidate"]["utility_score"],
        decayed_utility=case["candidate"]["decayed_utility"],
        staleness_floor=case["candidate"].get("staleness_floor", 0.2),
    )
    probes = [
        TransferProbe(
            probe_id=p["probe_id"],
            kind=p["kind"],
            query=p["query"],
            answer_without_memory=p["answer_without_memory"],
            answer_with_memory=p["answer_with_memory"],
        )
        for p in case["probes"]
    ]

    async def scripted_decide(rendered_context: dict[str, Any]) -> dict[str, Any]:
        has_memory = any(m["role"] == "system" and "Known fact:" in m["content"] for m in rendered_context["messages"])
        query = rendered_context["messages"][-1]["content"]
        probe = next(p for p in probes if p.query == query)
        return _normalized_text(probe.answer_with_memory if has_memory else probe.answer_without_memory)

    report = await evaluate_learning(candidate, probes, scripted_decide)
    return report.forward_transfer_ok, report.negative_transfer_ok


CaseRunner = Callable[[dict[str, Any]], Awaitable[tuple[bool, bool]]]

# Which suites this Runner can actually exercise offline today, and how. A suite named in
# evals/suites/ with no entry here (e.g. injection_suite — there is no real injection-resistance
# harness yet) reports every grader SKIPPED rather than a fabricated score.
CASE_RUNNERS: dict[str, CaseRunner] = {
    "deep_research_core": _run_deep_research_core_case,
    "learning_eval": _run_learning_eval_case,
}

# Each grader scores the (bool, bool) tuples every case in a suite's run produced — for
# deep_research_core that's (sufficiency_ok, citation_ok); for learning_eval it's
# (forward_transfer_ok, negative_transfer_ok). Both are "does every case's first/second flag hold",
# so the same two lambda shapes cover every suite registered so far.
GRADER_RUNNERS: dict[str, Callable[[list[tuple[bool, bool]]], float]] = {
    "coverage_grader": lambda outcomes: sum(1 for ok, _ in outcomes if ok) / len(outcomes),
    "citation_integrity_grader": lambda outcomes: sum(1 for _, ok in outcomes if ok) / len(outcomes),
    "forward_transfer_grader": lambda outcomes: sum(1 for ok, _ in outcomes if ok) / len(outcomes),
    "negative_transfer_grader": lambda outcomes: sum(1 for _, ok in outcomes if ok) / len(outcomes),
}


def _threshold_key(grader_name: str) -> str:
    return grader_name.removesuffix("_grader")


async def run_suite(suite_name: str, trials: int = 1) -> SuiteReport:
    suite = load_suite(suite_name)
    spec = suite["spec"]
    thresholds: dict[str, float] = spec["thresholds"]
    grader_names: list[str] = spec["graders"]

    report = SuiteReport(suite_name=suite_name, trials=trials)
    case_runner = CASE_RUNNERS.get(suite_name)

    if case_runner is None:
        for grader_name in grader_names:
            report.graders.append(
                GraderResult(
                    name=grader_name,
                    status="SKIPPED",
                    score=None,
                    threshold=thresholds.get(_threshold_key(grader_name)),
                    detail="no offline case runner registered for this suite yet — see backlog.md",
                )
            )
        return report

    cases = load_dataset(spec["dataset"])
    outcomes: list[tuple[bool, bool]] = []
    for _ in range(trials):
        for case in cases:
            outcomes.append(await case_runner(case))

    for grader_name in grader_names:
        threshold = thresholds.get(_threshold_key(grader_name))
        grade_fn = GRADER_RUNNERS.get(grader_name)
        if grade_fn is None:
            report.graders.append(
                GraderResult(grader_name, "SKIPPED", None, threshold, "no grader implementation registered")
            )
            continue

        score = grade_fn(outcomes)
        status = "PASS" if threshold is None or score >= threshold else "FAIL"
        report.graders.append(GraderResult(grader_name, status, score, threshold))

    return report


def format_report(report: SuiteReport) -> str:
    lines = [f"suite={report.suite_name} trials={report.trials} overall={report.status}"]
    for g in report.graders:
        score_str = "n/a" if g.score is None else f"{g.score:.3f}"
        threshold_str = "n/a" if g.threshold is None else f"{g.threshold:.3f}"
        detail = f" ({g.detail})" if g.detail else ""
        lines.append(f"  {g.name}: {g.status} score={score_str} threshold={threshold_str}{detail}")
    return "\n".join(lines)
