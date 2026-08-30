"""test_integrity_gate_blocks_missing_constraint — the acceptance test named in roadmap.md CTX-006.

The gate is the last check before a model call: it must catch a bug anywhere upstream (CTX-001..005)
that silently dropped a required constraint, left a dangling recall_id pointer, or produced a
context over the target's token budget — and block, not warn.
"""
from __future__ import annotations

import pytest

from aeon_context.integrity import IntegrityGate, IntegrityViolation
from aeon_context.lanes import ContextAssembler, Lane, LaneState


def _context_with_policy(policy_text: str):
    lanes = {Lane.L0_POLICY: LaneState(lane=Lane.L0_POLICY, entries=[{"text": policy_text}])}
    return ContextAssembler.assemble(lanes)


def test_integrity_gate_blocks_missing_constraint():
    rendered = _context_with_policy("policy: never execute shell.*")

    report = IntegrityGate.check(rendered, required_constraints=["policy: never reveal secrets"])

    assert report.ok is False
    assert "policy: never reveal secrets" in report.missing_constraints


def test_integrity_gate_enforce_raises_on_missing_constraint():
    rendered = _context_with_policy("policy: never execute shell.*")

    with pytest.raises(IntegrityViolation) as exc_info:
        IntegrityGate.enforce(rendered, required_constraints=["policy: never reveal secrets"])
    assert "never reveal secrets" in str(exc_info.value)


def test_integrity_gate_passes_when_every_constraint_is_present():
    rendered = _context_with_policy("policy: never reveal secrets. policy: never execute shell.*")

    report = IntegrityGate.check(
        rendered, required_constraints=["policy: never reveal secrets", "policy: never execute shell.*"]
    )

    assert report.ok is True
    assert report.missing_constraints == []


def test_integrity_gate_blocks_dangling_recall_id():
    lanes = {Lane.L3_EPISODIC: LaneState(lane=Lane.L3_EPISODIC, entries=[{"recall_id": "obs_ghost", "summary": "x"}])}
    rendered = ContextAssembler.assemble(lanes)

    report = IntegrityGate.check(rendered, known_recall_ids={"obs_real_one"})

    assert report.ok is False
    assert "obs_ghost" in report.dangling_recall_ids


def test_integrity_gate_accepts_a_known_recall_id():
    lanes = {Lane.L3_EPISODIC: LaneState(lane=Lane.L3_EPISODIC, entries=[{"recall_id": "obs_real_one", "summary": "x"}])}
    rendered = ContextAssembler.assemble(lanes)

    report = IntegrityGate.check(rendered, known_recall_ids={"obs_real_one"})

    assert report.ok is True
    assert report.dangling_recall_ids == []


def test_integrity_gate_blocks_over_token_budget():
    rendered = _context_with_policy("x" * 10_000)

    report = IntegrityGate.check(rendered, max_tokens=10)

    assert report.ok is False
    assert report.estimated_tokens > 10


def test_integrity_gate_enforce_reports_all_violations_together():
    lanes = {
        Lane.L0_POLICY: LaneState(lane=Lane.L0_POLICY, entries=[{"text": "policy: X"}]),
        Lane.L3_EPISODIC: LaneState(lane=Lane.L3_EPISODIC, entries=[{"recall_id": "obs_ghost", "summary": "x"}]),
    }
    rendered = ContextAssembler.assemble(lanes)

    with pytest.raises(IntegrityViolation) as exc_info:
        IntegrityGate.enforce(
            rendered,
            required_constraints=["policy: Y (missing)"],
            known_recall_ids=set(),
            max_tokens=1,
        )
    message = str(exc_info.value)
    assert "missing constraints" in message
    assert "dangling recall_ids" in message
    assert "over token budget" in message
