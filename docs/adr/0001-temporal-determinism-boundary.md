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
- Replay-based tests (`test_crash_resume_no_duplicate_write`, `test_graph_runtime_node_kinds`)
  become the primary acceptance evidence for RUN-002/RUN-004, not code review alone.

## Implementation note: `imports_passed_through()` must be declared in the `@workflow.defn` file

Discovered building RUN-002's Graph Runtime (python/aeon_worker/graph.py). Temporal's Python SDK
sandbox re-executes whichever module actually defines the `@workflow.defn` class under a restricted
import environment; `with workflow.unsafe.imports_passed_through(): import X` inside THAT file is
what tells the sandbox "reuse the real `X` module object, don't reload it." Declaring the same
`imports_passed_through()` block only in a *helper* module the workflow file imports (even one
already itself marked passthrough) is not equivalent — it does not propagate transitively to that
helper's own imports. The symptom is not an import error: it's Activity result decoding failing
deterministically with `NameError: name 'Any' is not defined` (or similar) inside
`get_type_hints()`, on every replay — which manifests as the workflow task retrying forever with
Temporal's backoff, indistinguishable from a hang unless you read the worker's stderr.

Rule: every workflow file (`workflows/*.py`) must directly declare `imports_passed_through()` for
every module whose types cross an Activity/Workflow payload boundary — `ExecuteToolInput`,
`ExecuteToolOutput`, `execute_tool_activity`, etc. — even if a helper module it imports already
imports those names itself. See `agent_run.py` and `graph_run.py` for the pattern; `graph.py`'s
module docstring carries the same note.
