# ADR-001: Workflow/Activity determinism boundary on Temporal

## Status
Accepted

## Context
The platform's central promise (roadmap.md §2.1, acceptance criterion in the spec §9: "a run
stopped by a process crash can resume from checkpoint and does not repeat an already-confirmed
write") requires a durable execution engine. We chose Temporal over a hand-rolled
Postgres-based supersteps engine or LangGraph's checkpointer (see plan discussion): Temporal
gives us checkpointing, replay, retries and timers for free, at the cost of a strict determinism
discipline.

The classic failure mode: someone calls a model, reads `datetime.now()`, or does anything else
non-deterministic directly inside workflow code. On replay, this produces a different history and
either crashes the workflow or silently diverges — undetected in dev, catastrophic in production.

## Decision
1. **Workflow code is deterministic only.** It contains: the graph cursor, the budget counters, the
   checkpoint/superstep boundaries, and `wait_condition` calls for approvals. It NEVER calls a model
   provider or a tool directly, and never touches wall-clock time except through Temporal's
   workflow-safe time APIs.
2. **All non-determinism lives in Activities.** `model.decide` is an Activity that returns a typed
   `Decision` (proto/schemas/decision.schema.json). The workflow validates and applies that
   Decision; it does not re-derive it.
3. **Every Activity with side effects is idempotent.** `idempotency_key = hash(run_id, node_id,
   step_seq, args_canonical)`. The Tool Gateway keeps a dedupe table keyed on this and returns the
   previously recorded result on retry, rather than re-executing.
4. **CI enforces this mechanically**: Temporal's Python SDK workflow sandbox (which restricts
   non-deterministic calls inside `@workflow.run` methods) is left enabled, never disabled via
   `workflow.unsafe`. A replay test exists per agent profile.

## Consequences
- An inference Activity that dies mid-call cannot resume mid-token; it retries from the start. We
  accept this and bound the blast radius with a low `maximumAttempts` and per-attempt cost
  accounting (so a flaky retry doesn't silently multiply spend).
- Any new node type in the Graph Runtime (RUN-002) must be reviewed for this boundary before merge.
- Replay-based tests (`test_crash_resume_no_duplicate_write`) become the primary acceptance
  evidence for RUN-004, not code review alone.
