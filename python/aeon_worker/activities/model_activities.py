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

from aeon_observability import inject_trace_context, tracer

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
    body = json.dumps(
        {
            "candidates": [{"provider": c.provider, "model": c.model, "priority": c.priority} for c in inp.candidates],
            "rendered_context": inp.rendered_context,
            "data_sensitivity": inp.data_sensitivity,
        }
    ).encode("utf-8")

    url = f"http://{DEFAULT_MODELGW_ADDR}/decide"
    # OTel GenAI semantic conventions, the same ones the Go gateways use (OBS-001): the operation is named
    # `chat` and the model attributes carry the gen_ai.* prefix, so a trace reads the same whichever side of
    # the seam produced the span.
    with tracer().start_as_current_span("chat") as span:
        span.set_attribute("gen_ai.operation.name", "chat")
        if inp.candidates:
            span.set_attribute("gen_ai.request.model", inp.candidates[0].model)
            span.set_attribute("gen_ai.system", inp.candidates[0].provider)
        if inp.data_sensitivity:
            span.set_attribute("aeon.data_sensitivity", inp.data_sensitivity)

        # THE LINE THAT MAKES ONE TRACE. Without it the gateway starts a root span and this run's spans end
        # up scattered across unrelated traces — each present, none connected.
        headers = inject_trace_context({"Content-Type": "application/json"})
        request = urllib.request.Request(url, data=body, headers=headers, method="POST")
        try:
            with urllib.request.urlopen(request, timeout=60) as response:  # noqa: S310 — fixed internal URL, not user input
                parsed = json.loads(response.read())
        except urllib.error.HTTPError as exc:
            detail = json.loads(exc.read()).get("error", exc.reason)
            span.record_exception(exc)
            raise ModelGatewayError(f"model gateway at {DEFAULT_MODELGW_ADDR} returned {exc.code}: {detail}") from exc
        except urllib.error.URLError as exc:
            span.record_exception(exc)
            raise ModelGatewayError(f"model gateway at {DEFAULT_MODELGW_ADDR} unreachable: {exc.reason}") from exc

        span.set_attribute("gen_ai.response.model", parsed.get("model", ""))
        span.set_attribute("aeon.provider_used", parsed.get("provider_used", ""))
        return DecideOutput(provider_used=parsed["provider_used"], model=parsed["model"], output=parsed["output"])


@activity.defn
async def decide_activity(inp: DecideInput) -> DecideOutput:
    return await call_model_gateway(inp)
