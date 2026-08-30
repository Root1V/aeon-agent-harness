"""Evidence Extractor (RAG-002): turns a retrieved document into EvidencePacket entries
(proto/schemas/evidence_packet.schema.json) — compaction CONDITIONED on the query/subtopic, never
a generic summary of the whole document. Every provenance field (claim_id, source_id, retrieved_at)
is runtime-generated here, never by whatever proposes the actual claim text (a model, in a full
deployment) — this module owns provenance; the extraction strategy only proposes claim+quote pairs,
and never gets to invent its own identity or timestamp.
"""
from __future__ import annotations

import uuid
from datetime import datetime, timezone
from typing import Protocol

from aeon_evidence.retrieval import ScoredDocument


class ClaimExtractionStrategy(Protocol):
    """Proposes (claim, quote) pairs from a document, conditioned on a query. In a full deployment
    this calls a model (question-conditioned compaction, per the spec); this module never assumes
    which — a model-backed strategy is a drop-in replacement behind this same Protocol."""

    def extract(self, query: str, document_text: str) -> list[tuple[str, str]]: ...  # [(claim, quote)]


class SentenceMatchExtractionStrategy:
    """A real, simple, non-model extraction strategy: sentences containing a query keyword become
    claims, verbatim, with the sentence itself as the quote — a claim is never allowed to say more
    than its quote literally supports, by construction, since claim IS the quote here. Useful on
    its own for keyword-heavy retrieval and as a fallback when no model is configured; a
    model-backed strategy replaces this behind the same Protocol without changing extract_evidence."""

    def extract(self, query: str, document_text: str) -> list[tuple[str, str]]:
        query_terms = {t.lower() for t in query.split() if len(t) > 2}
        sentences = [s.strip() for s in document_text.replace("\n", " ").split(".") if s.strip()]
        return [(s, s) for s in sentences if any(term in s.lower() for term in query_terms)]


def extract_evidence(
    query: str,
    subtopic_id: str,
    scored_document: ScoredDocument,
    strategy: ClaimExtractionStrategy,
    tenant_id: str | None = None,
) -> list[dict]:
    """Returns EvidencePacket-shaped dicts (proto/schemas/evidence_packet.schema.json), one per
    (claim, quote) the strategy proposes for this document. source_quality/confidence come from
    the retrieval score, clamped into the schema's [0, 1] range — RAG-001's hybrid score can exceed
    1.0 (term-frequency plus a phrase-match boost), which would otherwise produce an invalid
    packet."""
    packets = []
    for claim, quote in strategy.extract(query, scored_document.document.text):
        packet = {
            "claim_id": str(uuid.uuid4()),
            "subtopic_id": subtopic_id,
            "claim": claim,
            "quote": quote,
            "source_id": scored_document.document.id,
            "retrieved_at": datetime.now(timezone.utc).isoformat(),
            "source_quality": min(1.0, max(0.0, scored_document.score)),
            "confidence": min(1.0, max(0.0, scored_document.score)),
            "support": "SUPPORTS",
        }
        if tenant_id is not None:
            packet["tenant_id"] = tenant_id
        packets.append(packet)
    return packets
