"""Typed Compaction (CTX-004): the LAST resort in the context-size toolkit (roadmap.md's A1) — try
cache-stable ordering (CTX-002) and offload (CTX-003) first; compact only under a named trigger (a
hard budget still exceeded after those), and only in a way each lane's own fidelity allows. There
is deliberately no uniform "summarize everything" step: what compaction is even allowed to DO to a
lane's entries is dictated by that lane's fixed fidelity (aeon_context.lanes.LANE_FIDELITY), same
as rendering is.

- PINNED_EXACT (L0_POLICY): compaction is a no-op, always, unconditionally. This is the one
  invariant the whole module exists to protect — see test_pinned_exact_survives_stress.
- EVIDENCE_ATOMIC (L2_EVIDENCE): may drop whole entries to shrink, but a surviving entry is never
  reworded, merged, or truncated — "atomic" means the unit of compaction is a whole claim, not a
  sentence inside one. Dropping the oldest entries first is a placeholder priority; real relevance
  ranking is RAG-002/003's job once they exist.
- ADDRESSABLE (L3_EPISODIC): individual text entries may be truncated (already-addressable
  {recall_id} entries are left alone — they're already minimal).
- Everything else: naive whole-text truncation, pending each lane's own real fidelity story
  (CTX-005, RAG-001, MEM-*).
"""
from __future__ import annotations

from dataclasses import replace

from aeon_context.lanes import Fidelity, LaneState


def _compact_pinned_exact(state: LaneState, target_max_chars: int) -> LaneState:
    return state  # never touched, regardless of target — this is the whole point.


def _compact_evidence_atomic(state: LaneState, target_max_chars: int) -> LaneState:
    entries = list(state.entries)
    total = sum(len(str(e)) for e in entries)
    # Drop from the end until under budget or nothing optional is left. Surviving entries are
    # untouched — dropped entirely, never edited.
    while total > target_max_chars and len(entries) > 1:
        dropped = entries.pop()
        total -= len(str(dropped))
    return replace(state, entries=entries)


def _compact_addressable(state: LaneState, target_max_chars: int) -> LaneState:
    per_entry_budget = max(50, target_max_chars // max(1, len(state.entries)))
    new_entries = []
    for entry in state.entries:
        if "recall_id" in entry:
            new_entries.append(entry)  # already minimal — nothing to compact further
            continue
        text = entry.get("text", "")
        if len(text) > per_entry_budget:
            entry = {**entry, "text": text[:per_entry_budget].rstrip() + "…"}
        new_entries.append(entry)
    return replace(state, entries=new_entries)


def _compact_generic(state: LaneState, target_max_chars: int) -> LaneState:
    per_entry_budget = max(50, target_max_chars // max(1, len(state.entries)))
    new_entries = []
    for entry in state.entries:
        text = entry.get("text")
        if text is not None and len(text) > per_entry_budget:
            entry = {**entry, "text": text[:per_entry_budget].rstrip() + "…"}
        new_entries.append(entry)
    return replace(state, entries=new_entries)


_COMPACTORS = {
    Fidelity.PINNED_EXACT: _compact_pinned_exact,
    Fidelity.EVIDENCE_ATOMIC: _compact_evidence_atomic,
    Fidelity.ADDRESSABLE: _compact_addressable,
}


def compact_lane(state: LaneState, target_max_chars: int) -> LaneState:
    """Pure: returns a new LaneState, never mutates the input. Safe to call repeatedly (a "stress"
    suite calling this dozens of times against the same PINNED_EXACT lane must see the identical
    entries every time — see test_pinned_exact_survives_stress)."""
    compactor = _COMPACTORS.get(state.fidelity, _compact_generic)
    return compactor(state, target_max_chars)


def compact_lanes(lanes: dict, target_max_chars_per_lane: int) -> dict:
    """Applies compact_lane to every lane in the mapping, returning a new mapping."""
    return {lane: compact_lane(state, target_max_chars_per_lane) for lane, state in lanes.items()}
