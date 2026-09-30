"""Activities for MEM-003's Reflection: propose memory candidates after a run, and persist them.

WHY TWO ACTIVITIES AND NOT ONE. Reflecting is a model call — expensive, retryable, and free of
consequence if repeated. Writing candidates is an EFFECT. Temporal retries an Activity as a whole, so a
single activity doing both would re-run the model on a failed write and, worse, re-write on a failed
model call. The split is the same one ADR-001 draws everywhere else in this codebase: one boundary per
kind of failure.

IDEMPOTENCY IS THE REASON THE WRITE LOOKS THE WAY IT DOES. MemoryStore.Create assigns a RANDOM uuid when
the caller supplies no memory_id, and the insert carries no ON CONFLICT — so a retried write would create
a second, indistinguishable candidate and nothing would report it. Supplying a DETERMINISTIC memory_id
turns that silent duplicate into a unique-violation the store already maps to ErrAlreadyExists and the
handler already answers 409. So a retry is idempotent by construction, and 409 is treated as success
because it means precisely "this candidate is already recorded".
"""
from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass, field
from typing import Any

from temporalio import activity

from aeon_memory.reflection import MemoryCandidate, Reflector, ReflectionError, RunSummary
from aeon_observability import inject_trace_context
from aeon_worker.activities.model_activities import DecideCandidate, DecideInput, call_model_gateway

# The control plane hosts the memory surface (MEM-001/MEM-002). Unset means no memory write path is
# configured, which the write activity reports rather than silently skipping — the same rule TOOL-004
# applies to the tool gateway, and for the same reason: a deployment with no memory store must not be
# indistinguishable from one where reflection simply found nothing.
CONTROLPLANE_ADDR = os.environ.get("AEON_CONTROLPLANE_ADDR", "")


@dataclass
class ReflectInput:
    run_id: str
    query: str
    outcome: str
    report_text: str
    evidence_refs: list[str]
    model: str
    candidates: list[DecideCandidate]
    data_sensitivity: str = ""
    agent_manifest_ref: str = ""  # OBS-003b


@dataclass
class ReflectedCandidate:
    type: str
    scope: str
    content: str
    evidence_refs: list[str] = field(default_factory=list)
    source_run_ids: list[str] = field(default_factory=list)
    confidence: float = 0.0


@dataclass
class ReflectOutput:
    candidates: list[ReflectedCandidate] = field(default_factory=list)
    # Why nothing was proposed, when nothing was. Empty on success, so a caller can tell "the model
    # proposed no memories" from "reflection did not run" — two facts a bare empty list collapses.
    skipped_reason: str = ""


@dataclass
class WriteCandidatesInput:
    run_id: str
    tenant_id: str
    candidates: list[ReflectedCandidate]


@dataclass
class WriteCandidatesOutput:
    written: int = 0
    already_present: int = 0
    skipped_reason: str = ""


# The namespace for MEM-003's deterministic candidate ids. A fixed uuid, not derived from anything, so
# the same run and the same candidate always produce the same id across processes and deployments.
_CANDIDATE_NAMESPACE = uuid.UUID("6f3d2a1c-9b4e-5c7a-8d1f-2e6b0a4c8d35")


def candidate_memory_id(run_id: str, candidate: ReflectedCandidate) -> str:
    """A deterministic id for a candidate, so a retried write collides instead of duplicating.

    A UUIDv5 and not a hash string: memory_records.memory_id is a Postgres UUID column, and the first
    version returned `mem-<sha256 prefix>`, which the store rejected with `invalid input syntax for type
    uuid`. Found by running it against a real control plane — the value looked like an id and the schema
    disagreed.

    Derived from the run and the candidate's own CONTENT, never from its position in the list: a retry of
    the reflect activity asks the model again, and it may return the same candidates in a different
    order — which an index-based id would record as new memories.
    """
    name = "\u0000".join([run_id, candidate.type, candidate.scope, candidate.content])
    return str(uuid.uuid5(_CANDIDATE_NAMESPACE, name))


@activity.defn
async def reflect_activity(inp: ReflectInput) -> ReflectOutput:
    """Ask the model for reusable candidates from a finished run.

    A reflection failure returns a REASON rather than raising. The run's report is the deliverable and it
    is already produced by the time this runs; failing the workflow here would throw away a completed
    piece of work because an optional, post-hoc step did not parse. The reason travels so that "no
    candidates" and "reflection could not run" stay distinguishable in the result.
    """

    async def decide(rendered_context: dict[str, Any]) -> dict[str, Any]:
        result = await call_model_gateway(
            DecideInput(
                candidates=inp.candidates,
                rendered_context=rendered_context,
                data_sensitivity=inp.data_sensitivity,
                # OBS-003b: reflection is an EXTRA model call per run (see the `reflect` flag's note in
                # DeepResearchWorkflowInput — a caller who has not thought about memory should not
                # silently start paying for one). Attributing it to the run is what makes that cost
                # visible to whoever turned the flag on, instead of it landing in the unattributed pile.
                run_id=inp.run_id,
                agent_manifest_ref=inp.agent_manifest_ref,
            )
        )
        return result.output

    summary = RunSummary(
        run_id=inp.run_id,
        query=inp.query,
        outcome=inp.outcome,
        report_text=inp.report_text,
        evidence_refs=inp.evidence_refs,
    )
    try:
        proposed: list[MemoryCandidate] = await Reflector(model=inp.model).reflect(summary, decide)
    except ReflectionError as exc:
        # Ungrounded or unparseable output. NOT retried and not raised: the Reflector already refuses to
        # repair a candidate that cites evidence the run never produced, and asking the same model the
        # same question again is unlikely to change that.
        return ReflectOutput(skipped_reason=f"reflection rejected the model's output: {exc}")

    return ReflectOutput(
        candidates=[
            ReflectedCandidate(
                type=c.type,
                scope=c.scope,
                content=c.content,
                evidence_refs=list(c.evidence_refs),
                source_run_ids=list(c.source_run_ids),
                confidence=c.confidence,
            )
            for c in proposed
        ]
    )


@activity.defn
async def write_memory_candidates_activity(inp: WriteCandidatesInput) -> WriteCandidatesOutput:
    """Persist candidates through the control plane's governed write path.

    POST /memory/candidates and nothing else: MemoryStore.WriteCandidate forces status=CANDIDATE
    regardless of what is sent, so even a model that tried to smuggle in an ACTIVE memory writes a
    quarantine-pipeline candidate. This activity does not get to choose otherwise, which is the point.
    """
    if not inp.candidates:
        return WriteCandidatesOutput()
    if not CONTROLPLANE_ADDR:
        return WriteCandidatesOutput(
            skipped_reason="AEON_CONTROLPLANE_ADDR is not set, so there is no memory store to write to"
        )

    written = already = 0
    for candidate in inp.candidates:
        body = json.dumps(
            {
                "memory_id": candidate_memory_id(inp.run_id, candidate),
                "type": candidate.type,
                "scope": candidate.scope,
                "tenant_id": inp.tenant_id,
                "content": candidate.content,
                "source_run_ids": candidate.source_run_ids or [inp.run_id],
                "evidence_refs": candidate.evidence_refs,
                "confidence": candidate.confidence,
            }
        ).encode()
        request = urllib.request.Request(
            f"http://{CONTROLPLANE_ADDR}/memory/candidates",
            data=body,
            method="POST",
            headers=inject_trace_context({"Content-Type": "application/json"}),
        )
        try:
            with urllib.request.urlopen(request, timeout=30):
                written += 1
        except urllib.error.HTTPError as exc:
            if exc.code == 409:
                # Already recorded by an earlier attempt of this activity. Counted separately rather
                # than folded into `written`, because "we wrote three" and "we wrote one and two were
                # already there" describe different runs and only one of them is a retry.
                already += 1
                continue
            raise

    return WriteCandidatesOutput(written=written, already_present=already)
