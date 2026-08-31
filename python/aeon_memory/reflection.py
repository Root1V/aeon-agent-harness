"""Reflection (MEM-003): post-run candidate extraction — the REFLECT step of
STORE -> REFLECT -> ABSTRACT -> VALIDATE -> PROMOTE -> USE -> REFINE/FORGET (roadmap.md §4, F3).

Given a completed run's outcome, report and the evidence_refs that were actually available to it
(its Evidence Ledger's claim_ids), asks a model to propose reusable memory candidates — the same
"extract candidates of success/failure, never convert them directly into active memory" principle
the spec names. Grounding is enforced exactly like DR-004's Reporter: a candidate whose
evidence_refs cite anything outside what the run actually produced is rejected, not repaired —
Reflection must never let a model invent provenance for a memory the same way a Reporter must never
let it invent a citation.

This module never decides `status`: it hands back plain candidate dicts (type/scope/content/
evidence_refs/source_run_ids/confidence) with no `status` field at all. The only way any of this
ever reaches storage is MEM-002's `MemoryStore.WriteCandidate` (Go), which forces `status=CANDIDATE`
regardless of what a caller supplies — so even a compromised or hallucinating reflection pass can
never write anything but a quarantine-pipeline candidate. Deliberately pure, no Temporal import —
directly unit-testable with a fake `decide`, same as Planner/Researcher/Reporter (docs/adr/0001).
"""
from __future__ import annotations

import json
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any

MEMORY_TYPES = frozenset({"EPISODIC", "SEMANTIC", "PROCEDURAL", "CONSTRAINT"})
MEMORY_SCOPES = frozenset({"session", "user", "project", "tenant", "org"})

DecideFn = Callable[[dict[str, Any]], Awaitable[dict[str, Any]]]


class ReflectionError(ValueError):
    """Raised when the model's reflection doesn't parse, uses an invalid type/scope/confidence, or
    cites an evidence_ref the run never actually produced. Never auto-repaired — an ungrounded
    candidate is dropped from consideration entirely, not patched up to look grounded."""


@dataclass
class RunSummary:
    """The post-run facts Reflection is allowed to see — never chain-of-thought, only the run's
    outcome and the evidence_refs it actually produced (roadmap.md §6: "sin almacenar
    chain-of-thought")."""

    run_id: str
    query: str
    outcome: str  # "success" | "failure", the run's own sufficient/verified verdict
    report_text: str
    evidence_refs: list[str] = field(default_factory=list)


@dataclass
class MemoryCandidate:
    type: str
    scope: str
    content: str
    evidence_refs: list[str]
    source_run_ids: list[str]
    confidence: float


def build_reflection_request(run_summary: RunSummary, model: str) -> dict[str, Any]:
    """No `tools`/`tool_choice` key: Reflection only ever reasons over facts already in
    run_summary, same tool-less-by-construction guarantee as DR-004's Reporter."""
    evidence_block = "\n".join(f"- {ref}" for ref in run_summary.evidence_refs) or "(none)"
    instructions = (
        "You are the Reflection step for an agent harness. Given a completed run's outcome, "
        "propose zero or more reusable memory candidates a future run could benefit from — a "
        "successful strategy worth repeating, a constraint learned from a failure, or a durable "
        "fact worth remembering. You have NO tools available. Every candidate's evidence_refs must "
        "be drawn ONLY from the list below — never invent one. type must be one of "
        f"{sorted(MEMORY_TYPES)}; scope must be one of {sorted(MEMORY_SCOPES)}; confidence is a "
        "number in [0, 1]. If nothing is worth remembering, return an empty list. Respond with "
        'ONLY a JSON object matching: {"candidates": [{"type": string, "scope": string, '
        '"content": string, "evidence_refs": [string, ...], "confidence": number}, ...]}. No prose, '
        "no markdown fences.\n\nRun outcome: " + run_summary.outcome + "\nEvidence available:\n" + evidence_block
    )
    return {
        "model": model,
        "messages": [
            {"role": "system", "content": instructions},
            {"role": "user", "content": run_summary.query},
        ],
    }


def parse_reflection(raw_model_output: dict[str, Any], run_summary: RunSummary) -> list[MemoryCandidate]:
    try:
        content = raw_model_output["choices"][0]["message"]["content"]
    except (KeyError, IndexError, TypeError) as exc:
        raise ReflectionError(f"model output is not NormalizedChatResponse-shaped: {raw_model_output!r}") from exc

    try:
        doc = json.loads(content)
    except json.JSONDecodeError as exc:
        raise ReflectionError(f"model output is not valid JSON: {content!r}") from exc

    if not isinstance(doc, dict) or "candidates" not in doc or not isinstance(doc["candidates"], list):
        raise ReflectionError(f"reflection is missing a 'candidates' list: {doc!r}")

    allowed_refs = set(run_summary.evidence_refs)
    candidates: list[MemoryCandidate] = []
    for raw in doc["candidates"]:
        if not isinstance(raw, dict):
            raise ReflectionError(f"candidate is not an object: {raw!r}")

        mem_type = raw.get("type")
        if mem_type not in MEMORY_TYPES:
            raise ReflectionError(f"candidate has invalid type {mem_type!r}, must be one of {sorted(MEMORY_TYPES)}")

        scope = raw.get("scope")
        if scope not in MEMORY_SCOPES:
            raise ReflectionError(f"candidate has invalid scope {scope!r}, must be one of {sorted(MEMORY_SCOPES)}")

        content_text = raw.get("content")
        if not isinstance(content_text, str) or not content_text:
            raise ReflectionError(f"candidate content must be a non-empty string, got {content_text!r}")

        confidence = raw.get("confidence", 0.0)
        if not isinstance(confidence, (int, float)) or not (0.0 <= confidence <= 1.0):
            raise ReflectionError(f"candidate confidence must be in [0, 1], got {confidence!r}")

        evidence_refs = list(raw.get("evidence_refs", []))
        unlisted = [r for r in evidence_refs if r not in allowed_refs]
        if unlisted:
            raise ReflectionError(f"candidate cites evidence_ref(s) the run never produced: {unlisted!r}")

        candidates.append(
            MemoryCandidate(
                type=mem_type,
                scope=scope,
                content=content_text,
                evidence_refs=evidence_refs,
                source_run_ids=[run_summary.run_id],
                confidence=float(confidence),
            )
        )
    return candidates


class Reflector:
    """Calls a model via `decide` (the real path: aeon_worker.activities.model_activities.
    decide_activity through workflow.execute_activity) and validates its proposed candidates are
    grounded in the run's own evidence_refs."""

    def __init__(self, model: str) -> None:
        self.model = model

    async def reflect(self, run_summary: RunSummary, decide: DecideFn) -> list[MemoryCandidate]:
        request = build_reflection_request(run_summary, self.model)
        raw_output = await decide(request)
        return parse_reflection(raw_output, run_summary)
