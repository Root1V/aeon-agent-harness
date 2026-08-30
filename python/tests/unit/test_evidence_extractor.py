"""test_evidence_packet_schema_valid — the acceptance test named in roadmap.md RAG-002.

Proves extract_evidence() produces packets that are genuinely valid against
proto/schemas/evidence_packet.schema.json (not just "the right Python shape"), that extraction is
actually conditioned on the query (a document with no relevant sentences yields zero packets, not
a padded/fabricated one), and that a claim's quote is always verbatim text taken from the source
document — never something the extractor invented.
"""
from __future__ import annotations

import json
import uuid
from pathlib import Path

from jsonschema import Draft202012Validator

from aeon_evidence.extractor import SentenceMatchExtractionStrategy, extract_evidence
from aeon_evidence.retrieval import Document, ScoredDocument

REPO_ROOT = Path(__file__).resolve().parents[3]
SCHEMA = json.loads((REPO_ROOT / "proto" / "schemas" / "evidence_packet.schema.json").read_text())


def _scored(doc_id: str, text: str, score: float = 0.8) -> ScoredDocument:
    return ScoredDocument(document=Document(id=doc_id, text=text), score=score)


def test_evidence_packet_schema_valid_for_a_real_extraction():
    scored = _scored("doc1", "Whales are mammals. Whales live in the ocean. Tax law is unrelated.")
    packets = extract_evidence(
        query="whales ocean", subtopic_id=str(uuid.uuid4()), scored_document=scored, strategy=SentenceMatchExtractionStrategy()
    )

    assert len(packets) >= 1
    for packet in packets:
        Draft202012Validator(SCHEMA).validate(packet)  # raises on any violation


def test_evidence_packet_schema_valid_clamps_out_of_range_scores():
    # RAG-001's hybrid score can exceed 1.0 (term-frequency + phrase-match boost) — the schema
    # requires source_quality/confidence in [0, 1], so extract_evidence must clamp, not pass through.
    scored = _scored("doc1", "whales whales whales", score=1.5)
    packets = extract_evidence(
        query="whales", subtopic_id=str(uuid.uuid4()), scored_document=scored, strategy=SentenceMatchExtractionStrategy()
    )

    assert packets
    for packet in packets:
        assert 0.0 <= packet["source_quality"] <= 1.0
        assert 0.0 <= packet["confidence"] <= 1.0
        Draft202012Validator(SCHEMA).validate(packet)


def test_evidence_extraction_is_conditioned_on_the_query_not_generic():
    scored = _scored("doc1", "This document is entirely about tax law filing deadlines and forms.")
    packets = extract_evidence(
        query="cooking recipes", subtopic_id=str(uuid.uuid4()), scored_document=scored, strategy=SentenceMatchExtractionStrategy()
    )

    assert packets == [], "a document with no query-relevant content must yield zero packets, not a fabricated one"


def test_evidence_extraction_quote_is_always_verbatim_from_the_source():
    document_text = "The Eiffel Tower is in Paris. It was completed in 1889. Unrelated sentence about weather."
    scored = _scored("doc1", document_text)
    packets = extract_evidence(
        query="Eiffel Tower Paris", subtopic_id=str(uuid.uuid4()), scored_document=scored, strategy=SentenceMatchExtractionStrategy()
    )

    assert packets
    for packet in packets:
        assert packet["quote"] in document_text, "a quote must be text that literally exists in the source document"


def test_evidence_extraction_provenance_is_runtime_owned_not_strategy_supplied():
    scored = _scored("doc1", "whales are large ocean mammals")
    subtopic_id = str(uuid.uuid4())
    packets = extract_evidence(
        query="whales", subtopic_id=subtopic_id, scored_document=scored, strategy=SentenceMatchExtractionStrategy()
    )

    assert packets
    packet = packets[0]
    # source_id/subtopic_id come from the caller's real inputs, not from the extraction strategy.
    assert packet["source_id"] == "doc1"
    assert packet["subtopic_id"] == subtopic_id
    uuid.UUID(packet["claim_id"])  # must be a real, well-formed uuid
    assert packet["retrieved_at"]  # a real timestamp was stamped, not left blank
