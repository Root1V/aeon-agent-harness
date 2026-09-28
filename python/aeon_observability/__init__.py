"""Aeon's OpenTelemetry wiring for the Python side (OBS-001, extended past the Go gateways).

WHAT WAS MISSING AND WHY IT MATTERED. The Go gateways emit `chat`, `execute_tool` and `invoke_agent`
spans, and they reach Tempo — OBS-001's test proves that. What they did NOT form was a trace: OTel's
default propagator in Go is a no-op, nothing extracted an incoming context, and the Python Activities that
call those gateways emitted nothing at all. So one Deep Research run appeared as half a dozen unrelated
root spans, and OBS-001's test never noticed because it queried each span SEPARATELY and each existed.

INTEGRATION WITH ARGUS (github.com/Root1V/argus-observability-platform) IS CONFIGURATION, NOT A DEPENDENCY.
Argus ships `argus-sdk` and `argus-semconv`; Aeon uses neither. It is OTel-native and ingests plain OTLP on
4317/4318, so what the integration needs is an endpoint and two attribute names. That keeps a deployment
without Argus fully traced, and it follows the rule this codebase applies to every provider: nothing in
Aeon should require somebody else's SDK in order to work.

Its one convention that does change our configuration: APPLICATIONS EXPORT TO LOCALHOST. An app never
learns the central plane's address — a per-machine agent collector listens on localhost:4318 and forwards
to the gateway with its own auth and its own queueing. So the default here is localhost, and pointing
somewhere else is something a deployment does explicitly.
"""
from aeon_observability.tracing import (
    ARGUS_GUARDRAIL,
    ARGUS_HOT,
    GUARDRAIL_BUDGET_EXHAUSTED,
    current_traceparent,
    init_tracing,
    inject_trace_context,
    mark_guardrail,
    tracer,
)

__all__ = [
    "ARGUS_GUARDRAIL",
    "ARGUS_HOT",
    "GUARDRAIL_BUDGET_EXHAUSTED",
    "current_traceparent",
    "init_tracing",
    "inject_trace_context",
    "mark_guardrail",
    "tracer",
]
