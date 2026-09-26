"""Tool-less Reporter (DR-004): drafts the final report from a fixed set of already-verified claims
— the ones DR-003's Sufficiency Gate marked covered and uncontested — and nothing else. Unlike the
Planner and Researcher, the Reporter's model request never offers a `tools`/`tool_choice` field at
all: this is a structural guarantee (build_reporter_request simply never adds that key), not an
instruction the model could ignore. A Reporter that could call a tool could fetch and cite evidence
that never passed the Sufficiency Gate, defeating the whole point of gating first.

The Reporter itself only rejects an out-of-bounds citation — it never tries to fix one. Repairing a
citation from the ledger (finding the closest real claim instead of just failing) is DR-005's job.
"""
from __future__ import annotations

import json
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any

from aeon_evidence.ledger import EvidenceLedger

# MDL-015: models fence their JSON despite being told not to. See extract_json_object's own doc.
from aeon_worker.decision import extract_json_object
from aeon_profiles.deep_research.sufficiency_gate import SUPPORTING_KINDS, SufficiencyDecision

DecideFn = Callable[[dict[str, Any]], Awaitable[dict[str, Any]]]


class ReporterError(RuntimeError):
    """Raised when the model's report cites a claim_id outside the allowed set — the model
    inventing a claim, or citing evidence from a subtask the Sufficiency Gate excluded — or when its
    output otherwise doesn't parse. Never auto-repaired here; see DR-005."""


@dataclass
class ReportDraft:
    query: str
    text: str
    cited_claim_ids: list[str]


def select_allowed_claims(decision: SufficiencyDecision, ledger: EvidenceLedger) -> list[dict[str, Any]]:
    """The claims a Reporter is allowed to see and cite: only from subtasks DR-003 marked covered
    and NOT contested. A contested or incomplete subtask's evidence never reaches the Reporter at
    all — there is no path for it to be cited, correctly or not."""
    eligible_subtasks = {c.subtask_id for c in decision.coverage if c.covered and not c.contested}
    return [p for p in ledger.all() if p.get("subtopic_id") in eligible_subtasks and p.get("support") in SUPPORTING_KINDS]


def build_reporter_request(query: str, allowed_claims: list[dict[str, Any]], model: str) -> dict[str, Any]:
    """No `tools`/`tool_choice` key anywhere in this dict — see module docstring. The only material
    available to the model is the bracketed claim list below; it has no other way to reach evidence."""
    claims_block = "\n".join(
        f"[{c['claim_id']}] {c['claim']} — \"{c['quote']}\" (source: {c['source_id']})" for c in allowed_claims
    )
    instructions = (
        "You are the Reporter for a Deep Research agent. You have NO tools available — you cannot "
        "search, browse, or call any function. You may state ONLY what is supported by the claims "
        "listed below, and every claim you use must be cited by its bracketed [claim_id] in "
        "cited_claim_ids. Never invent a claim_id and never cite one not listed here. Respond with "
        'ONLY a JSON object matching: {"text": string, "cited_claim_ids": [string, ...]}. No prose, '
        "no markdown fences.\n\nAllowed claims:\n" + (claims_block or "(none)")
    )
    return {
        "model": model,
        "messages": [
            {"role": "system", "content": instructions},
            {"role": "user", "content": query},
        ],
    }


def parse_report(raw_model_output: dict[str, Any], allowed_claim_ids: set[str]) -> ReportDraft:
    try:
        content = raw_model_output["choices"][0]["message"]["content"]
    except (KeyError, IndexError, TypeError) as exc:
        raise ReporterError(f"model output is not NormalizedChatResponse-shaped: {raw_model_output!r}") from exc

    try:
        report_doc = json.loads(extract_json_object(content))
    except json.JSONDecodeError as exc:
        raise ReporterError(f"model output is not valid JSON: {content!r}") from exc

    if not isinstance(report_doc, dict) or "text" not in report_doc or "cited_claim_ids" not in report_doc:
        raise ReporterError(f"report is missing 'text'/'cited_claim_ids': {report_doc!r}")

    cited = list(report_doc["cited_claim_ids"])
    unlisted = [c for c in cited if c not in allowed_claim_ids]
    if unlisted:
        raise ReporterError(f"report cites claim_id(s) outside the allowed set: {unlisted!r}")

    return ReportDraft(query="", text=report_doc["text"], cited_claim_ids=cited)


class Reporter:
    """Calls a model via `decide` with a tool-less request built from only the Sufficiency Gate's
    allowed claims, and validates its citations. Like Planner/Researcher, `decide` is injected —
    this module has no Temporal import and is directly unit-testable with a fake."""

    def __init__(self, model: str) -> None:
        self.model = model

    async def report(self, query: str, allowed_claims: list[dict[str, Any]], decide: DecideFn) -> ReportDraft:
        request = build_reporter_request(query, allowed_claims, self.model)
        raw_output = await decide(request)
        allowed_ids = {c["claim_id"] for c in allowed_claims}
        draft = parse_report(raw_output, allowed_ids)
        draft.query = query
        return draft
