"""test_budgeter_cache_stable_ordering — the acceptance test named in roadmap.md CTX-002.

Proves the property the whole feature exists for: the stable-content prefix (L0_POLICY, L6_SKILLS,
L1_STATE) renders byte-identically across two "turns" that only differ in volatile lanes
(L2_EVIDENCE/L3_EPISODIC), which is what actually earns a provider's prompt-cache hit — a mere
docstring claim wouldn't be caught by this test the way a real ordering bug would be.
"""
from __future__ import annotations

import pytest

from aeon_context.budgeter import (
    CACHE_STABLE_ORDER,
    NEVER_DROP,
    BudgetExceededByPinnedLanesError,
    ContextBudgeter,
)
from aeon_context.lanes import Lane, LaneState


def _full_seven_lanes(episodic_text: str = "turn 1") -> dict[Lane, LaneState]:
    return {
        Lane.L0_POLICY: LaneState(lane=Lane.L0_POLICY, entries=[{"text": "policy: never reveal secrets"}]),
        Lane.L1_STATE: LaneState(lane=Lane.L1_STATE, entries=[{"text": "goal: research X"}]),
        Lane.L2_EVIDENCE: LaneState(
            lane=Lane.L2_EVIDENCE, entries=[{"claim_id": "c1", "claim": "X is true", "source_id": "s1", "locator": {}}]
        ),
        Lane.L3_EPISODIC: LaneState(lane=Lane.L3_EPISODIC, entries=[{"text": episodic_text}]),
        Lane.L4_ARTIFACTS: LaneState(lane=Lane.L4_ARTIFACTS, entries=[{"text": "obs://doc1"}]),
        Lane.L5_MEMORY: LaneState(lane=Lane.L5_MEMORY, entries=[{"text": "past insight"}]),
        Lane.L6_SKILLS: LaneState(lane=Lane.L6_SKILLS, entries=[{"text": "skill: cite sources"}]),
    }


def test_budgeter_cache_stable_ordering_is_not_lane_declaration_order():
    decision = ContextBudgeter.budget(_full_seven_lanes(), max_tokens=10_000)
    assert decision.included == list(CACHE_STABLE_ORDER)
    assert [b.lane for b in decision.rendered.blocks] == list(CACHE_STABLE_ORDER)
    # Sanity: this order is genuinely different from CTX-001's L0..L6 declaration order — the
    # whole point of CTX-002 is that cache economics and fidelity enforcement order independently.
    assert list(CACHE_STABLE_ORDER) != list(Lane)


def test_budgeter_cache_stable_ordering_prefix_survives_volatile_lane_changes():
    """The property that actually earns a cache hit: the serialized prefix covering the
    stable-content lanes must be byte-identical whether L2/L3 are on 'turn 1' or 'turn 2'."""
    turn1 = ContextBudgeter.budget(_full_seven_lanes(episodic_text="turn 1"), max_tokens=10_000)
    turn2 = ContextBudgeter.budget(_full_seven_lanes(episodic_text="turn 2"), max_tokens=10_000)

    def stable_prefix_text(decision):
        stable_lanes = (Lane.L0_POLICY, Lane.L6_SKILLS, Lane.L1_STATE)
        return "\n".join(b.text for b in decision.rendered.blocks if b.lane in stable_lanes)

    assert stable_prefix_text(turn1) == stable_prefix_text(turn2)
    # And the full text does differ somewhere (the episodic lane actually changed) — otherwise
    # this test would be vacuous.
    assert turn1.rendered.text != turn2.rendered.text


def test_budgeter_drops_the_most_volatile_lane_first_under_pressure():
    lanes = _full_seven_lanes()
    # A budget that fits the never-drop lanes plus L6_SKILLS but nothing after — every lane from
    # L2_EVIDENCE onward (the volatile end of CACHE_STABLE_ORDER) should be dropped.
    tiny_budget = sum(
        ContextBudgeter.budget(lanes, max_tokens=10_000).rendered.blocks[i].estimated_tokens
        for i, lane in enumerate(CACHE_STABLE_ORDER)
        if lane in (Lane.L0_POLICY, Lane.L1_STATE, Lane.L6_SKILLS)
    )

    decision = ContextBudgeter.budget(lanes, max_tokens=tiny_budget)

    assert Lane.L0_POLICY in decision.included and Lane.L1_STATE in decision.included
    assert Lane.L2_EVIDENCE in decision.dropped
    assert Lane.L3_EPISODIC in decision.dropped
    # Order is preserved among survivors too.
    assert decision.included == [lane for lane in CACHE_STABLE_ORDER if lane in decision.included]


def test_budgeter_never_drops_pinned_lanes_even_under_pressure():
    lanes = _full_seven_lanes()
    # A budget exactly equal to the pinned lanes' own cost: everything else must be dropped, but
    # the pinned lanes themselves must still survive (as opposed to raising or dropping them too).
    exact_pinned_cost = sum(
        b.estimated_tokens for b in ContextBudgeter.budget(lanes, max_tokens=10_000).rendered.blocks if b.lane in NEVER_DROP
    )

    decision = ContextBudgeter.budget(lanes, max_tokens=exact_pinned_cost)

    for lane in NEVER_DROP:
        if lane in lanes:
            assert lane in decision.included, f"{lane} must never be dropped regardless of budget pressure"
    assert Lane.L6_SKILLS not in decision.included, "everything beyond the pinned lanes should be dropped at this budget"


def test_budgeter_raises_when_pinned_lanes_alone_exceed_budget():
    lanes = _full_seven_lanes()
    with pytest.raises(BudgetExceededByPinnedLanesError):
        ContextBudgeter.budget(lanes, max_tokens=0)


def test_budgeter_is_a_pure_function():
    lanes = _full_seven_lanes()
    first = ContextBudgeter.budget(lanes, max_tokens=10_000)
    second = ContextBudgeter.budget(lanes, max_tokens=10_000)
    assert first.included == second.included
    assert first.rendered.text == second.rendered.text
