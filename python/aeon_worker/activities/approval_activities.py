"""OBS-010: a run waiting for a person is a recorded fact, not a silence.

WHAT WAS WRONG, and it was wrong in the direction that hides things. `INT-011` emits
`argus.outcome=suspended` on the POLICY decision — the moment Cedar says `require_approval` — and then
the run goes and waits inside `GraphRunWorkflow._await_approval` (RUN-005) emitting nothing at all.
A Temporal `wait_condition` is not a call, so no Activity runs, so no span exists: a run can sit
blocked on a human for hours and the only trace of it is a policy decision taken before the wait
began. "Why has this run not finished" was answerable by querying Temporal and by nothing else, which
is the exact shape of a fact that exists inside one system and nowhere an incident responder looks.

WE TOLD ARGUS THIS OURSELVES (2026-09-29): «el run todavía no se suspende de verdad por esa vía […]
os avisaremos cuando la primera espera larga sea observable». This is that.

WHY AN ACTIVITY AND NOT A SPAN IN THE WORKFLOW. Workflow code is replayed, so a span created there is
re-created on every replay unless something suppresses it. temporalio ships exactly that
(`ReplaySafeTracerProvider`), and it is marked experimental and wants to own the global tracer
provider — which `argus.init()` already owns. Emitting from an Activity needs none of that: a
completed Activity is not re-run on replay, so the span happens once by construction, and `argus.step`
works there like it works anywhere else. The cost is real and is measured, not assumed: this adds two
Activity commands to the history of every approval, which is a non-deterministic change for runs
already in flight. See `_await_approval` for what the measurement said.

WHY TWO RECORDS AND NOT ONE. A single span emitted when the wait ENDS would carry the true duration
and would be invisible for the whole time it matters: nothing answers "what is waiting right now"
until the waiting stops. So the wait is recorded twice under one event name, `approval.wait`, with
`argus.outcome` carrying the phase:

    suspended   the wait has begun and nobody has answered        (emitted before blocking)
    ok          granted                                            (emitted after the signal)
    denied      rejected, or decided against a different hash      (idem)
    timeout     the TTL elapsed and no decision ever arrived       (idem)

That mapping is Argus's, agreed in `/Victor/aeon_argus` on 2026-09-29, and `Step.outcome()` now
validates against `ARGUS_OUTCOME_VALUES` on their side, so a value we invent warns instead of
silently becoming un-aggregatable. "Which runs are waiting now" is `outcome=suspended` minus the
`aeon.approval.id`s that have a second record; both carry `argus.run.id`, so the join is available.

WHY THE WAIT DOES NOT GO IN `argus.duration_ms`. Their `Step._finalize` sets that field itself, from
`perf_counter()` around the span — so on this span it means "how long it took to write this record",
a millisecond or two, and it OVERWRITES anything set beforehand. A four-hour wait written into a field
that means something else is worse than no field. `aeon.approval.waited_ms` is ours and says what it is.

AND IT IS ABSENT, NOT ZERO, ON THE FIRST RECORD. Nobody has waited yet when the wait begins; writing 0
there would put "waited no time at all" and "has not finished waiting" in the same bucket, and the
aggregate that matters — how long approvals actually take — would be pulled toward zero by every run
still pending. Same three-state rule as MDL-014's token counters and OBS-008's unpriced costs.

NOT HOT, DELIBERATELY. No `argus.hot`, no guardrail, and the span status stays OK. A run waiting for a
person is the system working exactly as designed; Argus rejected our `approval-required` guardrail on
2026-09-29 for this reason (setting `argus.guardrail` at all asks for a page within two seconds), and
a long wait arriving through a different door would page just the same. Note for anyone extending
this: `argus.step(..., slo_ms=N)` sets `argus.hot` by itself when the span outlives N — passing an SLO
to a span that waits for a human would reintroduce the same page through a third door.
"""
from __future__ import annotations

from dataclasses import dataclass

from temporalio import activity

from aeon_observability import step_span

# Argus's outcome vocabulary, for the four phases of a wait. Imported from their semconv rather than
# spelled here, so a value that leaves their closed set fails our conformance test instead of shipping.
try:  # pragma: no cover - the fallback only runs where the SDK is not installed
    from argus_semconv.attributes import ARGUS_RUN_ID
except ImportError:  # pragma: no cover
    ARGUS_RUN_ID = "argus.run.id"

WAIT_EVENT = "approval.wait"

OUTCOME_SUSPENDED = "suspended"
OUTCOME_GRANTED = "ok"
OUTCOME_DENIED = "denied"
OUTCOME_EXPIRED = "timeout"


@dataclass
class ApprovalWaitInput:
    """One record of an approval wait.

    `waited_ms` is `None` on the record that OPENS the wait and an integer on the one that closes it —
    see the module docstring on why that is not a 0. It is computed from `workflow.now()` on the
    workflow side, never from a clock read here: this Activity can be retried minutes after the wait
    ended, and a duration measured at write time would report the retry, not the wait.
    """

    run_id: str
    approval_id: str
    node_id: str
    tool_call_hash: str
    outcome: str
    waited_ms: int | None = None


@activity.defn(name="record_approval_wait")
async def record_approval_wait_activity(inp: ApprovalWaitInput) -> None:
    """Write one `approval.wait` record. Returns nothing: this is a report, not a decision.

    It cannot fail the run either — the caller swallows errors — but it is still an Activity and not
    fire-and-forget, because a record that may or may not have been written is not a record.
    """
    with step_span(WAIT_EVENT) as step:
        step.set(**{ARGUS_RUN_ID: inp.run_id})
        step.set(**{
            "aeon.approval.id": inp.approval_id,
            "aeon.node.id": inp.node_id,
            # The hash the decision is bound to (RUN-005's parameter binding). Present on both records
            # so a granted wait can be matched to the exact parameters it was granted for.
            "aeon.approval.tool_call_hash": inp.tool_call_hash,
        })
        if inp.waited_ms is not None:
            step.set(**{"aeon.approval.waited_ms": inp.waited_ms})
        step.outcome(inp.outcome)
