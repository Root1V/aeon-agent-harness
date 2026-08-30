"""Retrieval Gateway (RAG-001): connectors, ACL, hybrid search, rerank, cache.

The one invariant that matters most here: **ACL is enforced by the gateway, never by a
connector**. A connector's job is raw retrieval — return every candidate document it has, whether
or not the caller may see it. The gateway is the single place access control is actually applied,
so adding a careless connector later can never accidentally leak a document past ACL — there is
exactly one code path a document must pass through to reach a caller, and that path always checks
scopes first.

Hybrid search here means two real (if simple) signals combined, not a placeholder: keyword overlap
(bag-of-words term frequency) and a phrase-match boost (rerank). Both run in pure Python — no
embedding model or network call — because RAG-001 is a routing/enforcement/caching contract, not
the retrieval quality bar; a real embedding-backed connector (pgvector, already in
deploy/compose/docker-compose.yml) is drop-in later behind the same Connector protocol.
"""
from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Protocol


@dataclass(frozen=True)
class Document:
    id: str
    text: str
    # Scopes allowed to see this document (e.g. "tenant:acme", "project:deep-research"). An empty
    # set means public — visible to any principal, matching the common "no ACL entry = public"
    # convention rather than the (much more dangerous) opposite default.
    acl: frozenset[str] = field(default_factory=frozenset)
    metadata: dict = field(default_factory=dict)


@dataclass(frozen=True)
class ScoredDocument:
    document: Document
    score: float


class Connector(Protocol):
    """A connector returns raw candidates for a query — it does not know about, and must not
    apply, ACL. That is the gateway's job, uniformly, for every connector."""

    def search(self, query: str) -> list[Document]: ...


@dataclass
class InMemoryConnector:
    """A real, working connector for tests and small deployments — not a mock. Production
    connectors (a real search index, pgvector) implement the same Protocol."""

    documents: list[Document]
    search_calls: int = field(default=0, init=False)

    def search(self, query: str) -> list[Document]:
        self.search_calls += 1
        return list(self.documents)  # the gateway does the actual matching/scoring/filtering


def _tokenize(text: str) -> list[str]:
    return re.findall(r"[a-z0-9]+", text.lower())


def _term_frequency_score(query_tokens: list[str], doc_tokens: list[str]) -> float:
    if not doc_tokens or not query_tokens:
        return 0.0
    doc_token_set = set(doc_tokens)
    matches = sum(1 for t in query_tokens if t in doc_token_set)
    return matches / len(query_tokens)


def _phrase_match_boost(query: str, text: str) -> float:
    # Rerank signal: an exact (case-insensitive) phrase match is strong evidence of relevance that
    # pure term-frequency overlap would under-rank against a document repeating individual words.
    return 0.5 if query.lower() in text.lower() else 0.0


def _hybrid_score(query: str, document: Document) -> float:
    query_tokens = _tokenize(query)
    doc_tokens = _tokenize(document.text)
    return _term_frequency_score(query_tokens, doc_tokens) + _phrase_match_boost(query, document.text)


class RetrievalGateway:
    def __init__(self) -> None:
        self._connectors: dict[str, Connector] = {}
        self._cache: dict[tuple[str, str, frozenset[str], int], list[ScoredDocument]] = {}

    def register_connector(self, name: str, connector: Connector) -> None:
        self._connectors[name] = connector

    def search(self, query: str, principal_scopes: frozenset[str], top_k: int = 10) -> list[ScoredDocument]:
        results: list[ScoredDocument] = []
        for name, connector in self._connectors.items():
            cache_key = (name, query, principal_scopes, top_k)
            if cache_key in self._cache:
                results.extend(self._cache[cache_key])
                continue

            candidates = connector.search(query)
            # ACL enforced HERE, once, for every connector — a document is visible if its acl is
            # empty (public) or intersects the principal's scopes. Never the other way around.
            visible = [doc for doc in candidates if not doc.acl or doc.acl & principal_scopes]
            scored = sorted(
                (ScoredDocument(document=doc, score=_hybrid_score(query, doc)) for doc in visible),
                key=lambda sd: sd.score,
                reverse=True,
            )[:top_k]

            self._cache[cache_key] = scored
            results.extend(scored)

        results.sort(key=lambda sd: sd.score, reverse=True)
        return results[:top_k]
