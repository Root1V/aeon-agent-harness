"""Evidence Ledger (RAG-003): stores EvidencePacket entries with provenance, dedupe, contradiction
grouping, and per-source quality aggregation.

The Ledger does not DETECT contradictions — that is a model's job (a future model-backed step,
same as RAG-002's real extraction strategy will eventually be model-backed). It PRESERVES and
GROUPS them once a caller identifies which existing claim_id(s) a new packet contradicts, exactly
as the spec requires: "contradicciones se preservan" — never silently resolved or dropped, never
invented from scratch by this module.
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass, field


class UnknownClaimError(Exception):
    """Raised when `contradicts` names a claim_id the ledger has never seen — a caller claiming a
    contradiction against a nonexistent claim is a bug, not something to silently ignore."""


@dataclass
class EvidenceLedger:
    _packets: dict[str, dict] = field(default_factory=dict)  # claim_id -> packet
    _dedup_index: dict[tuple[str, str], str] = field(default_factory=dict)  # (source_id, quote) -> claim_id

    def add(self, packet: dict, contradicts: list[str] | None = None) -> dict:
        """Adds a packet, or returns the existing one unchanged if it's a duplicate (same
        source_id+quote — a real, if simple, dedup key; not full semantic dedup, which would need
        a model). If `contradicts` names existing claim_ids, this packet and all of them join the
        same contradiction_group — merging into an existing group rather than creating a new one
        if any of the named claims are already grouped."""
        dedup_key = (packet["source_id"], packet["quote"])
        if dedup_key in self._dedup_index:
            return self._packets[self._dedup_index[dedup_key]]

        packet = dict(packet)  # never mutate the caller's dict
        if contradicts:
            group_id = self._resolve_or_create_group(contradicts)
            packet["contradiction_group"] = group_id
            for other_id in contradicts:
                self._packets[other_id]["contradiction_group"] = group_id

        self._packets[packet["claim_id"]] = packet
        self._dedup_index[dedup_key] = packet["claim_id"]
        return packet

    def _resolve_or_create_group(self, contradicts: list[str]) -> str:
        for other_id in contradicts:
            if other_id not in self._packets:
                raise UnknownClaimError(f"contradicts references unknown claim_id: {other_id}")

        existing_groups = {self._packets[oid].get("contradiction_group") for oid in contradicts}
        existing_groups.discard(None)
        if not existing_groups:
            return str(uuid.uuid4())

        group_id = sorted(existing_groups)[0]
        if len(existing_groups) > 1:
            # Two previously-separate groups are being linked by this new packet — merge them
            # into one rather than leaving the contradiction fragmented across two group ids.
            for other_packet in self._packets.values():
                if other_packet.get("contradiction_group") in existing_groups:
                    other_packet["contradiction_group"] = group_id
        return group_id

    def get(self, claim_id: str) -> dict:
        return self._packets[claim_id]

    def contradiction_group(self, group_id: str) -> list[dict]:
        return [p for p in self._packets.values() if p.get("contradiction_group") == group_id]

    def all(self) -> list[dict]:
        return list(self._packets.values())

    def source_quality(self, source_id: str) -> float | None:
        """Average source_quality across every packet seen from this source — the closest thing
        to a source reputation score until a real one exists."""
        scores = [p["source_quality"] for p in self._packets.values() if p["source_id"] == source_id]
        return sum(scores) / len(scores) if scores else None
