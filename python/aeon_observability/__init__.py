"""Aeon's telemetry seam, on Argus's SDK (OBS-006b).

See aeon_observability.tracing for why the SDK rather than plain OTLP — the short version is that
`argus-obs-semconv` carries the VALUES and companion attributes, not just the attribute keys, so
hand-rolling them produces telemetry that looks right and that their alert rules do not match.
"""
from aeon_observability.tracing import (
    ARGUS_GUARDRAIL,
    ARGUS_HOT,
    GUARDRAIL_APPROVAL_REQUIRED,
    GUARDRAIL_COST_BUDGET,
    GUARDRAIL_DESTINATION_UNKNOWN,
    GUARDRAIL_FAN_OUT,
    GUARDRAIL_POLICY_DENIED,
    GUARDRAIL_TOKEN_BUDGET,
    GUARDRAIL_TOOL_CALL_BUDGET,
    GUARDRAIL_TOOL_CALL_LOOP,
    chat_span,
    current_traceparent,
    init_tracing,
    inject_trace_context,
    mark_guardrail,
    run_span,
    shutdown,
    step_span,
    tool_span,
)

__all__ = [
    "ARGUS_GUARDRAIL",
    "ARGUS_HOT",
    "GUARDRAIL_APPROVAL_REQUIRED",
    "GUARDRAIL_COST_BUDGET",
    "GUARDRAIL_DESTINATION_UNKNOWN",
    "GUARDRAIL_FAN_OUT",
    "GUARDRAIL_POLICY_DENIED",
    "GUARDRAIL_TOKEN_BUDGET",
    "GUARDRAIL_TOOL_CALL_BUDGET",
    "GUARDRAIL_TOOL_CALL_LOOP",
    "chat_span",
    "current_traceparent",
    "init_tracing",
    "inject_trace_context",
    "mark_guardrail",
    "run_span",
    "shutdown",
    "step_span",
    "tool_span",
]
