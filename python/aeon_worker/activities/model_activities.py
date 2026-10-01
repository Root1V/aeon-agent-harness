"""Activities: model.decide (DR-001 onward) — the only place the Python worker is allowed to ask
for a model decision, per docs/adr/0001-temporal-determinism-boundary.md. It never talks to a
provider SDK itself: it calls the real Model Gateway (go/internal/api/model_gateway_handlers.go),
which owns all 5 provider adapters and their routing/fallback (docs/adr/0004) — unlike
tool_activities.py's execute_tool_activity, there is no local stand-in here, because model routing
and provider credentials only exist on the Go side.
"""
from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any

from temporalio import activity

from aeon_worker.outbound import service_headers
from aeon_observability import chat_span, set_run_identity

DEFAULT_MODELGW_ADDR = os.environ.get("AEON_MODELGW_ADDR", "localhost:9402")


class ModelGatewayError(RuntimeError):
    """Raised when the Model Gateway is unreachable, or reports a routing failure (every candidate
    failed, or a restricted call had no local candidate) — a runtime condition, not a programming
    error, so callers can catch it distinctly from a malformed request."""


@dataclass
class DecideCandidate:
    provider: str
    model: str
    priority: int


@dataclass
class DecideInput:
    candidates: list[DecideCandidate]
    rendered_context: dict[str, Any]
    data_sensitivity: str = ""
    # OBS-003b: who this call is FOR. The Model Gateway has accepted both since OBS-003 and
    # `FinOpsLedger` has recorded them since then, and no Python caller ever filled either — so the
    # ledger could aggregate "cost per model" and the other half of OBS-003's own title, cost per run
    # and per agent, had no data to show. Not the fields' fault and not the ledger's: the two ends
    # were built and the wire between them was never run.
    #
    # EMPTY MEANS "THE CALLER DID NOT SAY", and it survives as SQL NULL all the way down rather than
    # becoming an empty string. A call genuinely made outside any run — a bare `/decide` from a
    # script, an eval — is a real case, and it must stay distinguishable from a run that forgot to
    # identify itself, because the first is fine and the second is a bug in whoever called.
    run_id: str = ""
    agent_manifest_ref: str = ""


@dataclass
class DecideOutput:
    provider_used: str
    model: str
    output: dict[str, Any]


async def call_model_gateway(inp: DecideInput) -> DecideOutput:
    """The actual HTTP call to the Model Gateway — a plain function, not `@activity.defn`, so it
    can be called directly from another Activity's own body (e.g. aeon_worker.activities.
    deep_research_activities' per-stage Activities) without nesting a Temporal activity call inside
    an activity, which is not a thing Temporal supports. decide_activity below is the
    workflow-callable wrapper around this same logic."""
    payload: dict[str, Any] = {
        "candidates": [{"provider": c.provider, "model": c.model, "priority": c.priority} for c in inp.candidates],
        "rendered_context": inp.rendered_context,
        "data_sensitivity": inp.data_sensitivity,
    }
    # OMITTED WHEN EMPTY, not sent as "". The gateway's decideRequest treats a present-but-empty
    # string the same as absent today, so sending it would work — and the day it stops working the
    # failure would be a ledger full of rows attributed to the run whose id is the empty string.
    # Absent is what "we do not know" looks like on the wire.
    if inp.run_id:
        payload["run_id"] = inp.run_id
    if inp.agent_manifest_ref:
        payload["agent_manifest_ref"] = inp.agent_manifest_ref
    body = json.dumps(payload).encode("utf-8")

    url = f"http://{DEFAULT_MODELGW_ADDR}/decide"
    # OTel GenAI semantic conventions, the same ones the Go gateways use (OBS-001): the operation is named
    # `chat` and the model attributes carry the gen_ai.* prefix, so a trace reads the same whichever side of
    # the seam produced the span.
    # argus.genai("chat", ...) rather than a hand-built span: the operation literal, the span name and the
    # gen_ai.* attribute keys are all theirs, so this span is indistinguishable from one their own SDK
    # produced — which is what their dashboards and alert rules are written against.
    first = inp.candidates[0] if inp.candidates else None
    with chat_span(
        provider=first.provider if first else "unknown",
        request_model=first.model if first else None,
    ) as span:
        if inp.data_sensitivity:
            span.set("aeon.data_sensitivity", inp.data_sensitivity)
        # On the span as well as in the body, because the two answer different questions with the same
        # fact: the ledger answers "what did this run cost" after the fact, the trace answers "which
        # run is this call part of" while it is happening.
        # Through the seam, which owns both the attribute names and the fact that Argus's two span
        # types disagree about how `set` is called — see aeon_observability.set_run_identity.
        set_run_identity(span, run_id=inp.run_id, agent_manifest_ref=inp.agent_manifest_ref)

        # THE LINE THAT MAKES ONE TRACE. Without it the gateway starts a root span and this run's spans end
        # up scattered across unrelated traces — each present, none connected.
        headers = service_headers()
        request = urllib.request.Request(url, data=body, headers=headers, method="POST")
        try:
            with urllib.request.urlopen(request, timeout=60) as response:  # noqa: S310 — fixed internal URL, not user input
                parsed = json.loads(response.read())
        except urllib.error.HTTPError as exc:
            detail = json.loads(exc.read()).get("error", exc.reason)
            # Their error() carries the retryability, which is the part a consumer acts on. A 5xx from the
            # gateway is worth retrying and a 4xx is not, and that distinction is invisible in a recorded
            # exception.
            span.error("model_gateway_http_error", retryable=exc.code >= 500)
            raise ModelGatewayError(f"model gateway at {DEFAULT_MODELGW_ADDR} returned {exc.code}: {detail}") from exc
        except urllib.error.URLError as exc:
            span.error("model_gateway_unreachable", retryable=True)
            raise ModelGatewayError(f"model gateway at {DEFAULT_MODELGW_ADDR} unreachable: {exc.reason}") from exc

        # response()/usage()/backend() are their typed setters, so the attribute keys are the ones their
        # store indexes. usage() takes the three counters MDL-014 made nullable, and passing None keeps
        # "not reported" distinct from zero all the way into their pipeline instead of collapsing here.
        span.response(model=parsed.get("model"))
        usage = (parsed.get("output") or {}).get("usage") or {}
        span.usage(
            input_tokens=usage.get("prompt_tokens"),
            output_tokens=usage.get("completion_tokens"),
            cached_input_tokens=usage.get("cache_read_tokens"),
        )
        # backend() is where OBS-005's "which deployment answered" belongs: the provider that served is a
        # fact about the backend, not about the request.
        span.backend(backend_id=parsed.get("provider_used"))
        return DecideOutput(provider_used=parsed["provider_used"], model=parsed["model"], output=parsed["output"])


@activity.defn
async def decide_activity(inp: DecideInput) -> DecideOutput:
    return await call_model_gateway(inp)
