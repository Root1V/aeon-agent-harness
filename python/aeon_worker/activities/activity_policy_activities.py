"""Authorize an external activity before the workflow schedules it (VRT-AEON-001).

WHY THIS IS AN ACTIVITY AND NOT A CHECK IN WORKFLOW CODE: ADR-001. The policy engine is an HTTP
call, and workflow code does no I/O. The cost is two entries in the history per `activity` node —
the check, then the work — and that is the honest price of a decision that is visible in the replay
instead of being made in a process nobody can re-run.

AND IT IS THE ENFORCEMENT POINT, not an advisory read. For a tool, `/execute` both decides and
executes, so the gateway is in the data path. An external activity runs on the CONSUMER's worker, so
Aeon cannot be in the data path at all — the only thing Aeon controls is whether its own workflow
schedules the work. That makes this answer the decision, which is why the gateway enforces SEC-005
on this route (`caller.ActsAs`) even though plain `/check-policy` does not.

NO LOCAL MODE, deliberately, and this is where it differs from `tool_activities`. That module has a
`local-ledger` mode that records intent and executes nothing, which is a useful development state for
a tool. There is no equivalent here: an unreachable policy engine must not end in "scheduled
anyway". A run that cannot ask whether it may hand work to somebody else's worker does not hand it.
"""
from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any

from temporalio import activity
from temporalio.exceptions import ApplicationError

from aeon_worker.outbound import service_headers

def _toolgw_addr() -> str:
    """Read at CALL time, not import time.

    tool_activities captures its address into a module constant, which means a worker whose
    environment was completed after import silently has no gateway. Reading here costs nothing and
    removes a whole class of "configured but not picked up" — the same shape as the compose variable
    that was set and never read (TOOL-004).
    """
    return os.environ.get("AEON_TOOLGW_ADDR", "")


@dataclass
class ActivityPolicyInput:
    agent_manifest_ref: str
    activity_name: str
    task_queue: str
    # Whether the NODE gates on an approval. It travels with the question because the bundle can
    # answer "permitted, but only once a person says yes" (disposition require_approval), and that
    # answer is only satisfiable when the node carries the gate. Without this field the check cannot
    # tell "forbidden" from "permitted pending a person", and collapsing them would either refuse a
    # legitimate gated node or let an ungated one through.
    requires_approval: bool = False
    # VRT-AEON-005: the tenant of the RUN, so the gateway evaluates the bundle of whoever submitted
    # it rather than the bundle of this worker. Veritium measured the consequence on their own
    # deployment: with one worker, tenant B's permit was never consulted for B's runs.
    tenant: str = ""


@dataclass
class ActivityPolicyOutput:
    """What the bundle decided, carried back so the workflow's result records WHY it proceeded."""

    allowed: bool
    policy_id: str = ""
    disposition: str = ""


@activity.defn
async def check_activity_policy_activity(inp: ActivityPolicyInput) -> ActivityPolicyOutput:
    if not inp.agent_manifest_ref:
        raise ApplicationError(
            f"activity {inp.activity_name!r} has no agent_manifest_ref: Cedar evaluates against a "
            "principal, and work handed to an external worker without one cannot be governed",
            non_retryable=True,
        )
    addr = _toolgw_addr()
    if not addr:
        raise ApplicationError(
            f"activity {inp.activity_name!r} cannot be authorized: AEON_TOOLGW_ADDR is unset, so "
            "there is no policy engine to ask. An external activity is work handed to a worker Aeon "
            "does not own; scheduling it unauthorized is the one outcome this node must never have",
            non_retryable=True,
        )

    payload = json.dumps(
        {
            "agent_manifest_ref": inp.agent_manifest_ref,
            "activity_name": inp.activity_name,
            "task_queue": inp.task_queue,
        }
    ).encode()
    request = urllib.request.Request(
        f"http://{addr}/check-activity-policy", data=payload, headers=service_headers(run_tenant=inp.tenant)
    )

    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            body: dict[str, Any] = json.loads(response.read())
    except urllib.error.HTTPError as err:
        detail = err.read().decode(errors="replace")
        # 401/403 are not timing problems: the caller's own entry does not let it present this agent,
        # and retrying presents the same entry. 400 means the request shape is wrong, which is a
        # defect in this file rather than in the deployment.
        if err.code in (400, 401, 403):
            raise ApplicationError(
                f"activity {inp.activity_name!r} refused by the gateway ({err.code}): {detail}",
                non_retryable=True,
            ) from err
        raise ApplicationError(f"policy check returned {err.code}: {detail}") from err

    # BOTH SPELLINGS READ ON PURPOSE, and it is not defensiveness: `policy.Decision` in Go tags
    # `disposition` and `disposition_declared` in lowercase and leaves `Allowed`, `PolicyID` and
    # `CedarDecision` with no tag at all, so one response really does mix the two conventions.
    allowed = bool(body.get("Allowed", body.get("allowed", False)))
    policy_id = str(body.get("PolicyID", body.get("policy_id", "")) or "")
    disposition = str(body.get("Disposition", body.get("disposition", "")) or "")

    # `require_approval` IS A PERMIT whose answer is still "not now", so `Allowed` comes back false
    # for it (Decision.Allowed is `allowed && disposition.PermitsExecution()`). It has to be told
    # apart from a forbid before anything else, or a correctly gated node looks denied.
    if disposition == "require_approval":
        if not inp.requires_approval:
            # A contradiction between governance and authoring: the bundle wants a person and the
            # node does not stop for one. Refused rather than resolved — picking either side here
            # would decide a governance question in code, and the direction that "works" is the one
            # where the gate becomes decoration. The message names the exact edit either way.
            raise ApplicationError(
                f"activity {inp.activity_name!r} is permitted by {policy_id!r} only with approval, "
                f"but the node does not gate on one. Set requires_approval: true on this node, or "
                f"change the disposition in the bundle",
                non_retryable=True,
            )
        return ActivityPolicyOutput(allowed=True, policy_id=policy_id, disposition=disposition)

    if not allowed:
        # NOT RETRYABLE: an activity outside the bundle does not become permitted by asking again,
        # and a retry only delays a run that is already wrong — the same reasoning tool_activities
        # applies to a denied tool.
        raise ApplicationError(
            f"activity {inp.activity_name!r} on task queue {inp.task_queue!r} denied by policy for "
            f"{inp.agent_manifest_ref!r} (policy_id={policy_id!r}, disposition={disposition!r})",
            non_retryable=True,
        )

    return ActivityPolicyOutput(allowed=True, policy_id=policy_id, disposition=disposition)
