"""test_no_full_document_injection — the acceptance test named in roadmap.md CTX-003.

Spec §9 acceptance criterion: "un documento >50k tokens nunca se inyecta completo: el sistema
almacena el original y usa retrieval + evidence compaction." This test proves the storage/pointer
half of that (retrieval/compaction proper is RAG-001/CTX-004, still TODO): a large document never
appears, in full, anywhere in the assembled context — only a pointer does — and the original is
retrievable byte-for-byte from the observation store.
"""
from __future__ import annotations

import pytest

from aeon_context.lanes import ContextAssembler, Lane, LaneState
from aeon_context.offload import OFFLOAD_THRESHOLD_CHARS, FilesystemObservationStore, maybe_offload


@pytest.fixture
def store(tmp_path):
    return FilesystemObservationStore(root=tmp_path / "observations")


def test_no_full_document_injection_large_document_is_never_inlined(store):
    big_document = "The quick brown fox jumps over the lazy dog. " * 10_000  # well over threshold
    assert len(big_document) > OFFLOAD_THRESHOLD_CHARS

    result = maybe_offload(big_document, store)

    assert result.offloaded is True
    assert "text" not in result.entry, "an offloaded entry must not carry the full content under any key"
    assert big_document not in str(result.entry), "the full document must not appear anywhere in the entry"
    assert len(result.entry["summary"]) < 300


def test_no_full_document_injection_full_content_is_retrievable_byte_for_byte(store):
    big_document = "Section content varies here. " * 10_000
    result = maybe_offload(big_document, store)

    recalled = store.get(result.entry["recall_id"])

    assert recalled == big_document


def test_no_full_document_injection_survives_full_context_assembly(store):
    """The end-to-end property: even after building a full LaneState and running it through
    ContextAssembler.assemble(), the rendered context text must not contain the original document."""
    big_document = "Confidential findings paragraph. " * 10_000
    result = maybe_offload(big_document, store)

    lanes = {Lane.L3_EPISODIC: LaneState(lane=Lane.L3_EPISODIC, entries=[result.entry])}
    rendered = ContextAssembler.assemble(lanes)

    assert big_document not in rendered.text
    assert result.entry["recall_id"] in rendered.text, "the pointer itself must still be visible so a later recall can use it"


def test_no_full_document_injection_small_content_is_not_offloaded(store):
    small = "just a short tool result"
    result = maybe_offload(small, store)

    assert result.offloaded is False
    assert result.entry == {"text": small}


def test_no_full_document_injection_unknown_recall_id_raises(store):
    with pytest.raises(KeyError):
        store.get("obs_does_not_exist")
