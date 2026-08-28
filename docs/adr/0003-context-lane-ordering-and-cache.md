# ADR-003: Cache-stable lane ordering over default compaction

## Status
Accepted

## Context
The original specification's default posture was to compact context aggressively. 2026 practice
(Anthropic's context engineering guidance, and industry commentary on prompt caching economics)
shows that with prompt caching, keeping a stable prefix is often cheaper AND more faithful than
summarizing — compaction should be a deliberate response to a named constraint (a hard context
window limit, or a measured cost problem), not a default behavior.

Separately, reordering what lanes look like turn-to-turn invalidates the model provider's KV/prompt
cache, multiplying cost per call. This is not mentioned in the original spec and is the single
largest economic lever available to the platform.

## Decision
1. Preference order for handling context growth: (1) stable prefix + provider prompt cache,
   (2) offload to the observation store with an addressable pointer (CTX-003/CTX-005), (3) typed
   compaction (CTX-004) only when triggered by a named constraint (window limit reached, or cost
   budget exceeded).
2. Lanes are always assembled in the same order: L0 Policy → L6 Skills (active only) → L1 State →
   L2 Evidence → L3 Episodic, with the most volatile lane last. The Context Budgeter optimizes for
   cache-hit rate as a first-class metric, not only for token count.
3. Addressable Recall (CTX-005) ships before typed compaction in the roadmap (F1), specifically
   because a good recall mechanism makes compaction unnecessary in the majority of cases.

## Consequences
- `ContextLaneState.cache_stable` (proto/schemas/context_lane.schema.json) is a field the Budgeter
  reads, not decoration.
- Providers without any caching capability (many local-inference setups) fall back to more
  aggressive offloading rather than relying on a cache that isn't there — see ADR-004.
- Eval suites must include a cache-hit-rate metric alongside cost-per-success (OBS-003).
