"""Addressable Recall (CTX-005): the read half of Offload (CTX-003). Given a recall_id an earlier
offload produced, fetch content back and reinject only what's needed as a fresh lane entry — never
the whole original document again. `query`, when given, does a real (if simple) keyword-window
extraction rather than returning everything; full semantic search over recalled content is
RAG-001's job once it exists, not this module's.
"""
from __future__ import annotations

from dataclasses import dataclass

from aeon_context.offload import ObservationStore

DEFAULT_WINDOW_CHARS = 2000


class RecallNotFoundError(Exception):
    """Raised when a recall_id doesn't resolve to anything in the store — a stale or fabricated
    recall_id must fail loudly, not silently return empty content indistinguishable from a real
    no-match."""


@dataclass
class RecallResult:
    recall_id: str
    content: str
    truncated: bool
    matched: bool = True  # False only for a query that found nothing (see recall())


def recall(
    recall_id: str, store: ObservationStore, query: str | None = None, window_chars: int = DEFAULT_WINDOW_CHARS
) -> RecallResult:
    """Stable IDs (CTX-005's own name for itself): the same recall_id always resolves to the same
    stored content, and this function is idempotent — calling it twice with the same arguments
    against an unchanged store returns byte-identical results."""
    try:
        full_content = store.get(recall_id)
    except KeyError as exc:
        raise RecallNotFoundError(f"no observation for recall_id={recall_id!r}") from exc

    if query is None:
        if len(full_content) <= window_chars:
            return RecallResult(recall_id=recall_id, content=full_content, truncated=False)
        return RecallResult(recall_id=recall_id, content=full_content[:window_chars], truncated=True)

    idx = full_content.lower().find(query.lower())
    if idx == -1:
        return RecallResult(recall_id=recall_id, content="", truncated=False, matched=False)

    start = max(0, idx - window_chars // 2)
    end = min(len(full_content), idx + len(query) + window_chars // 2)
    window = full_content[start:end]
    truncated = start > 0 or end < len(full_content)
    return RecallResult(recall_id=recall_id, content=window, truncated=truncated)


def recall_as_lane_entry(recall_id: str, store: ObservationStore, query: str | None = None) -> dict:
    """recall() reshaped as a lane entry ready to append to an ADDRESSABLE lane (L3_EPISODIC) —
    {"text": ...} for the recalled fragment, distinct from the {"recall_id", "summary"} pointer
    form aeon_context.offload.maybe_offload produces. Both are valid ADDRESSABLE entries per
    aeon_context.lanes' _render_addressable."""
    result = recall(recall_id, store, query=query)
    return {"text": result.content}
