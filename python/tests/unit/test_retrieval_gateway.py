"""test_retrieval_acl_enforced — the acceptance test named in roadmap.md RAG-001.

The property that matters most: ACL is enforced by the GATEWAY, uniformly, regardless of what a
connector returns — a document a principal isn't scoped for must never reach them, even if it
would otherwise be the most relevant result, and even though the connector itself genuinely has it
(proving the gateway is the one filtering, not the connector conveniently omitting it).
"""
from __future__ import annotations

from aeon_evidence.retrieval import Document, InMemoryConnector, RetrievalGateway


def _gateway_with(*documents: Document) -> tuple[RetrievalGateway, InMemoryConnector]:
    connector = InMemoryConnector(documents=list(documents))
    gateway = RetrievalGateway()
    gateway.register_connector("test", connector)
    return gateway, connector


def test_retrieval_acl_enforced_excludes_documents_outside_principal_scope():
    secret_doc = Document(id="d1", text="acme quarterly revenue figures", acl=frozenset({"tenant:acme"}))
    other_doc = Document(id="d2", text="acme quarterly revenue figures", acl=frozenset({"tenant:other"}))
    gateway, connector = _gateway_with(secret_doc, other_doc)

    results = gateway.search("revenue figures", principal_scopes=frozenset({"tenant:acme"}))

    ids = {r.document.id for r in results}
    assert "d1" in ids
    assert "d2" not in ids


def test_retrieval_acl_enforced_even_when_the_excluded_doc_would_rank_first():
    # d2 is a much better keyword match than d1, but d2 is out of scope — ACL must win over
    # relevance, not just tiebreak against it.
    in_scope_but_weak = Document(id="d1", text="a mostly unrelated paragraph", acl=frozenset({"tenant:acme"}))
    out_of_scope_but_strong = Document(
        id="d2", text="exact query phrase exact query phrase exact query phrase", acl=frozenset({"tenant:other"})
    )
    gateway, _ = _gateway_with(in_scope_but_weak, out_of_scope_but_strong)

    results = gateway.search("exact query phrase", principal_scopes=frozenset({"tenant:acme"}))

    assert [r.document.id for r in results] == ["d1"]


def test_retrieval_acl_enforced_public_documents_have_no_acl_restriction():
    public_doc = Document(id="d1", text="public knowledge base entry")
    gateway, _ = _gateway_with(public_doc)

    results = gateway.search("public knowledge", principal_scopes=frozenset({"tenant:anyone"}))

    assert [r.document.id for r in results] == ["d1"]


def test_retrieval_acl_enforced_proves_the_connector_actually_had_the_excluded_doc():
    """Confirms ACL filtering happens at the gateway, not because the connector conveniently
    never returned the excluded document in the first place."""
    restricted_doc = Document(id="d1", text="restricted content", acl=frozenset({"tenant:acme"}))
    gateway, connector = _gateway_with(restricted_doc)

    excluded = gateway.search("restricted content", principal_scopes=frozenset({"tenant:other"}))
    assert excluded == []

    included = gateway.search("restricted content", principal_scopes=frozenset({"tenant:acme"}))
    assert len(included) == 1
    # Same connector instance both times — same underlying data, different scopes, different result.
    assert connector.documents[0].id == "d1"


def test_retrieval_gateway_hybrid_scoring_ranks_relevant_documents_first():
    relevant = Document(id="d1", text="the exact answer to the question about whales")
    irrelevant = Document(id="d2", text="a paragraph about tax law with no overlap")
    gateway, _ = _gateway_with(irrelevant, relevant)

    results = gateway.search("question about whales", principal_scopes=frozenset())

    assert results[0].document.id == "d1"
    assert results[0].score > results[1].score


def test_retrieval_gateway_caches_repeated_queries():
    doc = Document(id="d1", text="cached content")
    gateway, connector = _gateway_with(doc)

    gateway.search("cached content", principal_scopes=frozenset())
    gateway.search("cached content", principal_scopes=frozenset())

    assert connector.search_calls == 1, "an identical (query, scopes) search must hit the cache, not the connector, on the second call"


def test_retrieval_gateway_cache_is_scoped_per_principal():
    """Caching must never leak across principals: the same query cached for one scope set must
    not be served to a different scope set that should see different (or no) results."""
    doc = Document(id="d1", text="scoped content", acl=frozenset({"tenant:acme"}))
    gateway, connector = _gateway_with(doc)

    acme_results = gateway.search("scoped content", principal_scopes=frozenset({"tenant:acme"}))
    other_results = gateway.search("scoped content", principal_scopes=frozenset({"tenant:other"}))

    assert len(acme_results) == 1
    assert len(other_results) == 0
    assert connector.search_calls == 2, "different principal scopes must not share a cache entry"
