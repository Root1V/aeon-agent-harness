"""TOOL-009's write half: a run parks what it produced where a later call can fetch it.

WHY A WRITER SHIPS WITH THE READER. `artifact.read` reads artifacts, so implementing it alone would
have produced a reader over an empty store — both ends built and the wire never run, which is the
defect this project has found in its own work three times this week (OBS-003b's cost columns, MEM-003's
reflection, DR-001's pipeline). Nothing in the deployment produced an artifact before this module: the
CTX-003 offload store is a Python module with no caller, and MinIO used to run in the reference stack while
not one line of Go or Python talks to it.

SO WHAT IS AN ARTIFACT. Content a run produced and parked OUTSIDE its own context and outside
Temporal's history. The first one is the finished Deep Research report, which until now existed only
inside the workflow result — bounded by Temporal's payload limit and reachable only through Temporal.
An artifact is reachable by id, by a later step of the same run or by a person.

THE ID IS DETERMINISTIC, for the reason MEM-003's candidate ids are: a retried Activity must not create
a second artifact. uuid5 over (run_id, name) is pure, so it is the same id on every attempt and on
every replay, and the workflow can carry it in its result without having to wait for the write.
"""
from __future__ import annotations

import os
import re
import uuid
from dataclasses import dataclass
from pathlib import Path

from temporalio import activity

# A fixed namespace so the id is stable across processes and releases. Same construction as
# aeon_worker.activities.memory_activities._CANDIDATE_NAMESPACE, and a different value so a run's
# report and its memory candidate can never collide into one id.
_ARTIFACT_NAMESPACE = uuid.UUID("6f1a5d0e-0b4c-4a7e-9a1f-2c8d3b5e7a90")

# What the gateway reads and this writes. Unset means no artifact store is configured, which is
# REPORTED and not silently skipped: a run whose report was never parked anywhere must not look like one
# whose report was parked successfully (the same rule TOOL-004 applies to a missing tool gateway).
ARTIFACT_ROOT = os.environ.get("AEON_ARTIFACT_ROOT", "")

# auth's own tenant shape (go/internal/auth.tenantPattern), repeated here because the tenant becomes
# a directory name. Kept strict rather than sanitised: a name that does not match is refused, not
# rewritten into something that does.
_TENANT_DIR = re.compile(r"^[a-z][a-z0-9-]{0,62}$")

# The tenant an artifact is parked under when the RUN's tenant is unknown.
#
# WHY A FALLBACK AND NOT A REFUSAL, found by CI rather than reasoned about. The first version of
# GOV-001g refused an empty tenant, and that broke every Deep Research run: `DeepResearchWorkflow` is
# started by the Python SDK (`client.start_workflow`) and NOT by `POST /runs`, so it has no Temporal
# memo and therefore no run tenant — no Go service starts it, so this is production and not just the
# test that caught it. A report that is never parked because its run did not come through the Run
# Controller is a regression, not isolation.
#
# AND IT IS THE SAME FALLBACK EVERY OTHER SURFACE ALREADY MAKES. `effectiveTenant` answers a request
# with no X-Aeon-Run-Tenant with the CALLER's tenant, which for a run's step is this worker — so
# policy, cost and the journal all land in the worker's tenant when the run's is unknown. Refusing
# here made artifacts the one surface that behaved differently, and that inconsistency is what CI
# surfaced.
#
# THE MEMO ALWAYS WINS. This is only reached when there is nothing to lose to: a run started through
# the Run Controller carries its tenant and is parked under it. A deployment where ONE worker executes
# several tenants' runs must rely on that memo (see GOV-001f); if such a worker also starts runs
# directly through the SDK, those land here together, which is why this is operator configuration and
# not a constant — the operator who shares a worker is the one who has to name its tenant.
WORKER_TENANT = os.environ.get("AEON_WORKER_TENANT", "default")


def artifact_id(run_id: str, name: str) -> str:
    """The id a run reports and `artifact.read` accepts.

    `art_` plus 32 hex characters, which is what go/internal/toolexec.ArtifactIDPattern matches. The
    prefix exists so an id says which writer produced it: `obs_` is CTX-003's offload store, which has
    used that prefix since it was written.
    """
    return "art_" + uuid.uuid5(_ARTIFACT_NAMESPACE, f"{run_id}/{name}").hex


@dataclass
class WriteArtifactInput:
    run_id: str
    name: str
    content: str
    # GOV-001g: the tenant of the RUN, which decides WHICH artifact store this is parked in.
    #
    # It is not an attribution field. The store is <root>/<tenant>/<id>, and the gateway's
    # `artifact.read` opens a per-tenant os.Root, so the directory IS the isolation. Before this,
    # every tenant's artifacts shared one flat directory and an id is
    # `uuid5(FIXED_NAMESPACE, "<run_id>/<name>")` — deterministic, with the namespace a constant in
    # this repository and the name a literal ("report.md"). A tenant that had seen another tenant's
    # run id could compute the id of its finished report and read it. Measured through the real
    # gateway before the fix.
    #
    # Empty means the run's tenant is unknown (a run started before the memo existed), and the
    # artifact is NOT written: a report parked where nobody will look for it is worse than one the
    # output says was not parked. The reason travels in `note`.
    tenant: str = ""


@dataclass
class WriteArtifactOutput:
    artifact_id: str
    # Why it was not written, when it was not. Empty on success, so a caller can tell "parked" from
    # "nowhere to park it" — two facts an absent id would collapse.
    note: str = ""
    bytes_written: int = 0
    # True when an artifact with this id already held DIFFERENT content. The original is kept: a reader
    # may already have seen it, and overwriting would make what they read unreproducible. Same reasoning
    # as the Tool Gateway's ArgsDiverged, which refuses a reused idempotency key with new arguments.
    diverged: bool = False


@activity.defn
async def write_artifact_activity(inp: WriteArtifactInput) -> WriteArtifactOutput:
    """Park `content` as an artifact. Never raises: the run's deliverable is already produced.

    Failing a finished run because an artifact store is unconfigured or full would throw away completed
    work for a step that is, by construction, after the work — the same trade reflect_activity refuses.
    The reason travels in `note`.
    """
    aid = artifact_id(inp.run_id, inp.name)
    if not ARTIFACT_ROOT:
        return WriteArtifactOutput(artifact_id=aid, note="AEON_ARTIFACT_ROOT is not set, so there is nowhere to park artifacts")
    # Empty means "this run did not come through the Run Controller", which is legitimate and gets
    # the worker's own tenant. Anything else that is not a tenant name is REFUSED and reported, never
    # written one level up: that would put it exactly where the shared directory used to be —
    # readable by every tenant — while the run's output said it was parked successfully. The tenant
    # is a path component here, so this is the line between a name and a path: `..` is not a tenant.
    tenant = inp.tenant or WORKER_TENANT
    if not _TENANT_DIR.match(tenant):
        return WriteArtifactOutput(
            artifact_id=aid,
            note=f"the artifact store tenant would be {tenant!r}, which is not a tenant name, so "
            "there is nowhere to park this",
        )

    root = Path(ARTIFACT_ROOT) / tenant
    target = root / aid
    try:
        root.mkdir(parents=True, exist_ok=True)
        if target.exists():
            existing = target.read_text(encoding="utf-8", errors="replace")
            if existing == inp.content:
                # The ordinary retry: same id, same bytes, nothing to do. Reported as a success with the
                # byte count, because "already there" and "just written" are the same fact to a reader.
                return WriteArtifactOutput(artifact_id=aid, bytes_written=len(inp.content.encode()))
            return WriteArtifactOutput(
                artifact_id=aid,
                note="an artifact with this id already holds different content; the original was kept",
                bytes_written=len(existing.encode()),
                diverged=True,
            )
        # Written to a temporary name and renamed, so a reader never sees a half-written artifact.
        # rename(2) within one directory is atomic, and a crash between the two leaves a `.partial` file
        # rather than a truncated artifact that reads as complete.
        tmp = root / (aid + ".partial")
        tmp.write_text(inp.content, encoding="utf-8")
        tmp.replace(target)
        return WriteArtifactOutput(artifact_id=aid, bytes_written=len(inp.content.encode()))
    except OSError as exc:
        activity.logger.warning("artifact %s not written: %s", aid, exc)
        return WriteArtifactOutput(artifact_id=aid, note=f"could not write the artifact: {exc}")
