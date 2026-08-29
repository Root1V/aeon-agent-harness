"""Typed Context Lanes (CTX-001): the seven fidelity-typed lanes the Context Assembler renders
into a model-ready prompt (roadmap.md §3 / the spec's lane table). Each lane's fidelity is a fixed
property of the lane itself, never a per-call configuration — misrendering something that's
supposed to be PINNED_EXACT as if it were summarizable is exactly the failure mode this module
exists to make structurally impossible, not just documented against.

Scope: lane composition and fidelity enforcement only. Budgeting/ordering for prompt-cache
stability is CTX-002; offloading large tool I/O is CTX-003; typed compaction under a named trigger
is CTX-004; addressable recall is CTX-005 — none of those exist yet (roadmap.md, all `TODO`).

ContextAssembler.assemble() is a pure function: same lanes in, byte-identical rendering out, every
time. That purity is what makes context reproducible under replay (docs/adr/0001's determinism
principle, applied here to context the same way it applies to workflow code) and what makes the
fidelity policy actually testable without a model in the loop.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any


class Lane(str, Enum):
    L0_POLICY = "L0_POLICY"
    L1_STATE = "L1_STATE"
    L2_EVIDENCE = "L2_EVIDENCE"
    L3_EPISODIC = "L3_EPISODIC"
    L4_ARTIFACTS = "L4_ARTIFACTS"
    L5_MEMORY = "L5_MEMORY"
    L6_SKILLS = "L6_SKILLS"


class Fidelity(str, Enum):
    PINNED_EXACT = "PINNED_EXACT"
    STRUCTURED = "STRUCTURED"
    EVIDENCE_ATOMIC = "EVIDENCE_ATOMIC"
    ADDRESSABLE = "ADDRESSABLE"
    SUMMARIZABLE = "SUMMARIZABLE"
    CANONICAL_EXTERNAL = "CANONICAL_EXTERNAL"
    RETRIEVAL_ON_DEMAND = "RETRIEVAL_ON_DEMAND"
    PROGRESSIVE_DISCLOSURE = "PROGRESSIVE_DISCLOSURE"


# Fixed by lane identity — never configurable per call. There is no parameter that would let a
# caller accidentally render L0 (policy/constraints) as SUMMARIZABLE.
LANE_FIDELITY: dict[Lane, Fidelity] = {
    Lane.L0_POLICY: Fidelity.PINNED_EXACT,
    Lane.L1_STATE: Fidelity.STRUCTURED,
    Lane.L2_EVIDENCE: Fidelity.EVIDENCE_ATOMIC,
    Lane.L3_EPISODIC: Fidelity.ADDRESSABLE,
    Lane.L4_ARTIFACTS: Fidelity.CANONICAL_EXTERNAL,
    Lane.L5_MEMORY: Fidelity.RETRIEVAL_ON_DEMAND,
    Lane.L6_SKILLS: Fidelity.PROGRESSIVE_DISCLOSURE,
}


class LaneFidelityViolation(Exception):
    """Raised when a lane's entries don't satisfy the structural invariant its fixed fidelity
    requires — e.g. an L2_EVIDENCE entry with no source_id. Fails fast rather than silently
    assembling evidence that a later verification step (DR-005 Citation Verifier) couldn't check."""


@dataclass
class LaneState:
    """Mirrors proto/schemas/context_lane.schema.json. `lane` determines `fidelity` (see
    LANE_FIDELITY) — callers never set fidelity directly, so it can't drift from the lane."""

    lane: Lane
    entries: list[dict[str, Any]] = field(default_factory=list)
    cache_stable: bool = True

    @property
    def fidelity(self) -> Fidelity:
        return LANE_FIDELITY[self.lane]


@dataclass
class RenderedBlock:
    lane: Lane
    fidelity: Fidelity
    text: str
    estimated_tokens: int


@dataclass
class RenderedContext:
    blocks: list[RenderedBlock]

    @property
    def text(self) -> str:
        return "\n".join(b.text for b in self.blocks)

    @property
    def estimated_tokens(self) -> int:
        return sum(b.estimated_tokens for b in self.blocks)


def _estimate_tokens(text: str) -> int:
    # Deliberately crude (chars/4): real tokenization is provider-specific (ADR-004), and this
    # estimate only needs to be stable and monotonic for future budgeting (CTX-002), not exact.
    return max(1, len(text) // 4) if text else 0


def _render_pinned_exact(state: LaneState) -> str:
    parts = []
    for entry in state.entries:
        if "text" not in entry:
            raise LaneFidelityViolation(f"{state.lane}: PINNED_EXACT entry missing required 'text' field: {entry}")
        parts.append(entry["text"])
    return "\n".join(parts)


def _render_evidence_atomic(state: LaneState) -> str:
    required = ("claim_id", "claim", "source_id")
    parts = []
    for entry in state.entries:
        missing = [f for f in required if f not in entry]
        if missing:
            raise LaneFidelityViolation(f"{state.lane}: EVIDENCE_ATOMIC entry missing {missing}: {entry}")
        locator = entry.get("locator", {})
        parts.append(f"[{entry['claim_id']}] {entry['claim']} (source={entry['source_id']} locator={locator})")
    return "\n".join(parts)


def _render_addressable(state: LaneState) -> str:
    parts = []
    for entry in state.entries:
        if "recall_id" in entry:
            parts.append(f"[recall:{entry['recall_id']}] {entry.get('summary', '')}")
        elif "text" in entry:
            parts.append(entry["text"])
        else:
            raise LaneFidelityViolation(f"{state.lane}: ADDRESSABLE entry needs 'text' or 'recall_id': {entry}")
    return "\n".join(parts)


def _render_generic(state: LaneState) -> str:
    # STRUCTURED / CANONICAL_EXTERNAL / RETRIEVAL_ON_DEMAND / PROGRESSIVE_DISCLOSURE / SUMMARIZABLE:
    # each has its own future home (CTX-004/005, RAG-001, MEM-*) with its own invariant to enforce
    # once that lands; for now, just render 'text' if present.
    return "\n".join(entry.get("text", str(entry)) for entry in state.entries)


_RENDERERS = {
    Fidelity.PINNED_EXACT: _render_pinned_exact,
    Fidelity.EVIDENCE_ATOMIC: _render_evidence_atomic,
    Fidelity.ADDRESSABLE: _render_addressable,
}


class ContextAssembler:
    """Pure: assemble() depends only on its argument, never on wall-clock time, randomness, or any
    hidden state."""

    @staticmethod
    def assemble(lanes: dict[Lane, LaneState]) -> RenderedContext:
        blocks = []
        for lane in Lane:  # fixed enum declaration order — deterministic regardless of dict order
            state = lanes.get(lane)
            if state is None:
                continue
            renderer = _RENDERERS.get(state.fidelity, _render_generic)
            text = renderer(state)
            blocks.append(RenderedBlock(lane=lane, fidelity=state.fidelity, text=text, estimated_tokens=_estimate_tokens(text)))
        return RenderedContext(blocks=blocks)
