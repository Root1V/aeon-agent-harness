"""Unit tests for the idempotency primitives used by every side-effecting Activity (RUN-004).
These are pure/fast — no Temporal server needed — unlike test_crash_resume.py.
"""
from __future__ import annotations

import tempfile
from pathlib import Path

from aeon_worker.idempotency import EffectsLedger, derive_idempotency_key


def test_derive_idempotency_key_is_deterministic():
    key1 = derive_idempotency_key("run-1", "node-0", 0, {"b": 2, "a": 1})
    key2 = derive_idempotency_key("run-1", "node-0", 0, {"a": 1, "b": 2})
    assert key1 == key2, "key must not depend on dict key order (canonical JSON)"


def test_derive_idempotency_key_differs_by_step():
    key_step0 = derive_idempotency_key("run-1", "node-0", 0, {"a": 1})
    key_step1 = derive_idempotency_key("run-1", "node-0", 1, {"a": 1})
    assert key_step0 != key_step1


def test_effects_ledger_records_once():
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / "ledger.json"
        ledger = EffectsLedger(str(path))
        key = derive_idempotency_key("run-1", "node-0", 0, {"x": 1})

        dedup1, result1 = ledger.record_once(key, {"status": "written"})
        assert dedup1 is False
        assert ledger.count(key) == 1

        dedup2, result2 = ledger.record_once(key, {"status": "written-again-would-be-a-bug"})
        assert dedup2 is True, "second call with the same key must be deduplicated"
        assert result2 == result1, "deduplicated call must return the ORIGINAL recorded result"
        assert ledger.count(key) == 1, "must still be exactly one write"


def test_effects_ledger_survives_reopen():
    """The crash-resume test relies on this: a fresh process opening the same ledger path must
    see writes made by a prior (crashed) process."""
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / "ledger.json"
        key = derive_idempotency_key("run-1", "node-0", 0, {"x": 1})

        EffectsLedger(str(path)).record_once(key, {"status": "written"})

        reopened = EffectsLedger(str(path))
        assert reopened.count(key) == 1
        dedup, result = reopened.record_once(key, {"status": "should-not-overwrite"})
        assert dedup is True
        assert result["status"] == "written"
