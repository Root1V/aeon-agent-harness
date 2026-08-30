"""Context Budgeter (CTX-002): decides which lanes to include and in what order, optimizing for
prompt-cache hit rate ahead of raw token count (roadmap.md's A2: "con prompt caching, conservar
suele ser más barato y más fiel que resumir"). See docs/adr/0003.

Cache-stable order (fixed, never reordered per call): L0_POLICY -> L6_SKILLS -> L1_STATE ->
L2_EVIDENCE -> L3_EPISODIC -> L4_ARTIFACTS -> L5_MEMORY. Whatever changes most turn-to-turn goes
last, so the longest possible prefix stays byte-identical across calls and hits the provider's
prompt cache. This is a *different* fixed order than aeon_context.lanes.ContextAssembler's L0..L6
enum order — CTX-001's assemble() is about fidelity enforcement and is order-agnostic by lane
identity; this module is specifically about cache economics.
"""
from __future__ import annotations

from dataclasses import dataclass

from aeon_context.lanes import ContextAssembler, Lane, LaneState, RenderedContext

# Fixed cache-stable order: stable-content lanes first, most volatile last. Never reordered based
# on content or call site — reordering is exactly what invalidates a provider's prompt cache.
CACHE_STABLE_ORDER: tuple[Lane, ...] = (
    Lane.L0_POLICY,
    Lane.L6_SKILLS,
    Lane.L1_STATE,
    Lane.L2_EVIDENCE,
    Lane.L3_EPISODIC,
    Lane.L4_ARTIFACTS,
    Lane.L5_MEMORY,
)

# Lanes that may never be dropped under budget pressure: L0 is a hard constraint (PINNED_EXACT
# exists precisely so it's never negotiable), and L1 is the model's own understanding of the task
# — dropping it silently would make every other lane meaningless.
NEVER_DROP = frozenset({Lane.L0_POLICY, Lane.L1_STATE})


class BudgetExceededByPinnedLanesError(Exception):
    """Raised when L0_POLICY + L1_STATE alone (the lanes that can never be dropped) already
    exceed the token budget. No amount of dropping optional lanes fixes this — the caller must
    raise the budget or shrink the pinned content itself."""


@dataclass
class BudgetDecision:
    included: list[Lane]  # in CACHE_STABLE_ORDER
    dropped: list[Lane]
    rendered: RenderedContext


class ContextBudgeter:
    """Selects and orders lanes for a target token budget. Ordering is always
    CACHE_STABLE_ORDER; budgeting only decides which optional (non-NEVER_DROP) lanes survive,
    dropping the most volatile ones first — which also happens to be the right relevance
    prioritization, not just the right cache-economics one."""

    @staticmethod
    def budget(lanes: dict[Lane, LaneState], max_tokens: int) -> BudgetDecision:
        # Render every present lane once (cheap; assemble() is pure) to get real size estimates.
        full = ContextAssembler.assemble(lanes)
        by_lane = {b.lane: b for b in full.blocks}

        pinned_tokens = sum(by_lane[lane].estimated_tokens for lane in NEVER_DROP if lane in by_lane)
        if pinned_tokens > max_tokens:
            raise BudgetExceededByPinnedLanesError(
                f"L0_POLICY+L1_STATE alone need {pinned_tokens} tokens, budget is {max_tokens}"
            )

        # Reserve the pinned lanes' cost up front and only let optional lanes compete for what's
        # left. Without this, a small optional lane occurring earlier in CACHE_STABLE_ORDER than a
        # NEVER_DROP lane (e.g. L6_SKILLS before L1_STATE) could get greedily included on the
        # assumption there was room, and the unconditionally-included pinned lane would then push
        # total usage over max_tokens — order in CACHE_STABLE_ORDER must never affect whether the
        # budget is actually respected.
        available_for_optional = max_tokens - pinned_tokens

        included: list[Lane] = []
        dropped: list[Lane] = []
        used_optional = 0
        for lane in CACHE_STABLE_ORDER:
            if lane not in by_lane:
                continue
            cost = by_lane[lane].estimated_tokens
            if lane in NEVER_DROP:
                included.append(lane)
            elif used_optional + cost <= available_for_optional:
                included.append(lane)
                used_optional += cost
            else:
                dropped.append(lane)

        rendered = RenderedContext(blocks=[by_lane[lane] for lane in CACHE_STABLE_ORDER if lane in included])
        return BudgetDecision(included=included, dropped=dropped, rendered=rendered)
