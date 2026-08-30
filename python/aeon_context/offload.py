"""Offload (CTX-003): a large tool input/result never enters the context directly — it's written
to an observation store and the context gets a small pointer instead (an ADDRESSABLE-fidelity
entry: {recall_id, summary} — see aeon_context.lanes' L3_EPISODIC handling). Addressable Recall
(CTX-005, still TODO) is what lets a later tool call fetch the full content back by recall_id; this
module only does the offload half.

OFFLOAD_THRESHOLD_CHARS is about *characters*, not tokens — an exact token count would need the
same crude heuristic aeon_context.lanes._estimate_tokens already uses, so the threshold only needs
to be a stable, conservative trigger point derived from that same ratio, not an exact count.
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Protocol

# ~50k tokens at the ~4 chars/token ratio aeon_context.lanes._estimate_tokens uses — matches the
# spec's own example ("un documento >50k tokens nunca se inyecta completo").
OFFLOAD_THRESHOLD_CHARS = 50_000 * 4


class ObservationStore(Protocol):
    def put(self, content: str) -> str: ...  # returns an observation_id
    def get(self, observation_id: str) -> str: ...


@dataclass
class FilesystemObservationStore:
    """Real, not a mock: writes to local disk. A production deployment points this at MinIO/S3
    instead (same Protocol, see deploy/compose/docker-compose.yml's minio service) — this
    implementation is what a dev/test environment uses without that infra running."""

    root: Path

    def __post_init__(self) -> None:
        self.root.mkdir(parents=True, exist_ok=True)

    def put(self, content: str) -> str:
        observation_id = f"obs_{uuid.uuid4().hex}"
        (self.root / observation_id).write_text(content)
        return observation_id

    def get(self, observation_id: str) -> str:
        path = self.root / observation_id
        if not path.exists():
            raise KeyError(f"no such observation: {observation_id}")
        return path.read_text()


def _summarize(content: str, max_chars: int = 200) -> str:
    # A one-line preview, not a semantic summary — that would need a model call, and offload must
    # work without one (it happens on the write path, often before any model call this turn).
    one_line = " ".join(content.split())
    if len(one_line) <= max_chars:
        return one_line
    return one_line[:max_chars].rstrip() + "…"


@dataclass
class OffloadResult:
    offloaded: bool
    entry: dict  # ready to drop straight into a LaneState.entries list


def maybe_offload(
    content: str, store: ObservationStore, threshold_chars: int = OFFLOAD_THRESHOLD_CHARS
) -> OffloadResult:
    """Returns a lane entry: the raw {"text": content} if under threshold, or an addressable
    pointer {"recall_id": ..., "summary": ...} — with the content actually written to `store` — if
    over. Once over threshold, the returned entry never contains the full content; that's exactly
    the property test_no_full_document_injection checks."""
    if len(content) <= threshold_chars:
        return OffloadResult(offloaded=False, entry={"text": content})

    observation_id = store.put(content)
    return OffloadResult(offloaded=True, entry={"recall_id": observation_id, "summary": _summarize(content)})
