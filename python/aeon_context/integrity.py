"""Context Integrity Gate (CTX-006): the last check before a model call. Verifies the ALREADY-
ASSEMBLED context (after CTX-001..005 have all run) still carries every constraint it's supposed
to, that every addressable reference inside it actually resolves, and that it fits the target's
token limit. This gate is not itself a source of any of those guarantees — CTX-001's fidelity
enforcement and CTX-005's recall are — it's the last line of defense that catches a bug ANYWHERE
upstream that would otherwise silently drop a PINNED_EXACT constraint or leave a dangling
recall_id pointer, before that broken context ever reaches a model.
"""
from __future__ import annotations

import re
from dataclasses import dataclass, field

from aeon_context.lanes import RenderedContext

_RECALL_POINTER_RE = re.compile(r"\[recall:([^\]]+)\]")


class IntegrityViolation(Exception):
    """Raised by IntegrityGate.enforce() when the context must not go to the model as-is. The run
    loop must treat this as a hard stop, not a warning — the gate runs *before* the model call."""


@dataclass
class IntegrityReport:
    ok: bool
    missing_constraints: list[str] = field(default_factory=list)
    dangling_recall_ids: list[str] = field(default_factory=list)
    estimated_tokens: int = 0
    max_tokens: int | None = None


class IntegrityGate:
    @staticmethod
    def check(
        rendered: RenderedContext,
        required_constraints: list[str] | None = None,
        known_recall_ids: set[str] | None = None,
        max_tokens: int | None = None,
    ) -> IntegrityReport:
        full_text = rendered.text
        required_constraints = required_constraints or []

        missing = [c for c in required_constraints if c not in full_text]

        dangling: list[str] = []
        if known_recall_ids is not None:
            for match in _RECALL_POINTER_RE.finditer(full_text):
                recall_id = match.group(1)
                if recall_id not in known_recall_ids:
                    dangling.append(recall_id)

        estimated = rendered.estimated_tokens
        over_budget = max_tokens is not None and estimated > max_tokens

        return IntegrityReport(
            ok=not missing and not dangling and not over_budget,
            missing_constraints=missing,
            dangling_recall_ids=dangling,
            estimated_tokens=estimated,
            max_tokens=max_tokens,
        )

    @staticmethod
    def enforce(
        rendered: RenderedContext,
        required_constraints: list[str] | None = None,
        known_recall_ids: set[str] | None = None,
        max_tokens: int | None = None,
    ) -> IntegrityReport:
        report = IntegrityGate.check(rendered, required_constraints, known_recall_ids, max_tokens)
        if not report.ok:
            reasons = []
            if report.missing_constraints:
                reasons.append(f"missing constraints: {report.missing_constraints}")
            if report.dangling_recall_ids:
                reasons.append(f"dangling recall_ids: {report.dangling_recall_ids}")
            if report.max_tokens is not None and report.estimated_tokens > report.max_tokens:
                reasons.append(f"over token budget: {report.estimated_tokens} > {report.max_tokens}")
            raise IntegrityViolation("; ".join(reasons))
        return report
