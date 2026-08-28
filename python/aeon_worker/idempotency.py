"""Idempotency key derivation and the (test-scope) effects ledger.

Mirrors the real design in roadmap.md §2.1 / docs/adr/0001: every Activity with side effects is
keyed by `idempotency_key = hash(run_id, node_id, step_seq, args_canonical)`. In production this
ledger lives in the Tool Gateway's Postgres dedupe table (go/internal/store); here it is a small
JSON-file-backed store used by the worker's own idempotent Activities and by the integration test
in tests/integration/test_crash_resume.py, which needs the ledger to survive a hard process crash
of the worker (a JSON file on disk does; an in-memory dict would not).
"""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
from typing import Any


def derive_idempotency_key(run_id: str, node_id: str, step_seq: int, args: dict[str, Any]) -> str:
    """hash(run_id, node_id, step_seq, args_canonical) — see docs/adr/0001-temporal-determinism-boundary.md."""
    canonical = json.dumps(
        {"run_id": run_id, "node_id": node_id, "step_seq": step_seq, "args": args},
        sort_keys=True,
        separators=(",", ":"),
    )
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


class EffectsLedger:
    """File-backed dedupe table. Not for production use — see go/internal/store for the real one.

    A retry that reaches `record_once` with a key already present returns the previously recorded
    result without re-running the side effect: this is what satisfies "a crashed run, on resume,
    does not repeat an already-confirmed write" (spec §9 / roadmap.md RUN-004).
    """

    def __init__(self, path: str | os.PathLike[str]):
        self.path = Path(path)
        if not self.path.exists():
            self.path.write_text("{}")

    def _read(self) -> dict[str, Any]:
        try:
            return json.loads(self.path.read_text())
        except (json.JSONDecodeError, FileNotFoundError):
            return {}

    def record_once(self, key: str, result: dict[str, Any]) -> tuple[bool, dict[str, Any]]:
        """Returns (deduplicated, result). deduplicated=True means this call did NOT write —
        the result returned is the one recorded by a prior attempt."""
        ledger = self._read()
        if key in ledger:
            return True, ledger[key]
        ledger[key] = result
        self.path.write_text(json.dumps(ledger, indent=2))
        return False, result

    def count(self, key: str) -> int:
        """Number of writes recorded under `key` — always 0 or 1 by construction of record_once,
        exposed for tests to assert the write really happened exactly once."""
        return 1 if key in self._read() else 0
