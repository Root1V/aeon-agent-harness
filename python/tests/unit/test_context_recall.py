"""test_addressable_recall_roundtrip — the acceptance test named in roadmap.md CTX-005.

Proves the round trip that makes offload (CTX-003) actually useful mid-run: a recall_id resolves
back to real content (not a stub), a query narrows to a small window around a match instead of
returning everything, an unknown id fails loudly, and recall is stable/idempotent — the same
recall_id + query always resolves the same way against an unchanged store.
"""
from __future__ import annotations

import pytest

from aeon_context.lanes import ContextAssembler, Lane, LaneState
from aeon_context.offload import FilesystemObservationStore, maybe_offload
from aeon_context.recall import RecallNotFoundError, recall, recall_as_lane_entry


@pytest.fixture
def store(tmp_path):
    return FilesystemObservationStore(root=tmp_path / "observations")


def _offloaded_document(store, needle: str, position_fraction: float = 0.5) -> tuple[str, str]:
    """Builds a document large enough to offload, with `needle` embedded partway through, offloads
    it, and returns (recall_id, needle)."""
    filler = "The quick brown fox jumps over the lazy dog. " * 6000
    split_at = int(len(filler) * position_fraction)
    document = filler[:split_at] + f" {needle} " + filler[split_at:]
    result = maybe_offload(document, store)
    assert result.offloaded is True
    return result.entry["recall_id"], document


def test_addressable_recall_roundtrip_without_query_returns_a_bounded_window(store):
    recall_id, document = _offloaded_document(store, needle="IRRELEVANT_HERE")

    result = recall(recall_id, store)

    assert result.truncated is True
    assert len(result.content) < len(document)
    assert result.content == document[: len(result.content)]  # a real prefix, not fabricated


def test_addressable_recall_roundtrip_with_query_finds_a_small_window_around_the_match(store):
    recall_id, document = _offloaded_document(store, needle="THE_SPECIFIC_FACT_WE_WANT", position_fraction=0.7)

    result = recall(recall_id, store, query="THE_SPECIFIC_FACT_WE_WANT")

    assert result.matched is True
    assert "THE_SPECIFIC_FACT_WE_WANT" in result.content
    assert len(result.content) < len(document) / 10, "a query recall must be a small window, not the whole document"


def test_addressable_recall_roundtrip_query_with_no_match_is_honest_about_it(store):
    recall_id, _ = _offloaded_document(store, needle="something")

    result = recall(recall_id, store, query="THIS_STRING_DOES_NOT_APPEAR_ANYWHERE")

    assert result.matched is False
    assert result.content == ""


def test_addressable_recall_roundtrip_unknown_id_fails_loudly(store):
    with pytest.raises(RecallNotFoundError):
        recall("obs_totally_fabricated", store)


def test_addressable_recall_roundtrip_is_stable_and_idempotent(store):
    recall_id, _ = _offloaded_document(store, needle="STABLE_NEEDLE")

    first = recall(recall_id, store, query="STABLE_NEEDLE")
    second = recall(recall_id, store, query="STABLE_NEEDLE")

    assert first.content == second.content
    assert first.truncated == second.truncated


def test_addressable_recall_roundtrip_reinjects_as_a_valid_lane_entry(store):
    recall_id, document = _offloaded_document(store, needle="REINJECT_ME")

    entry = recall_as_lane_entry(recall_id, store, query="REINJECT_ME")
    lanes = {Lane.L3_EPISODIC: LaneState(lane=Lane.L3_EPISODIC, entries=[entry])}
    rendered = ContextAssembler.assemble(lanes)

    assert "REINJECT_ME" in rendered.text
    assert document not in rendered.text, "recall must reinject only the fragment, never the full original"
