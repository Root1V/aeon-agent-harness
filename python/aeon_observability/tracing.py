"""Tracer setup, context propagation and the Argus guardrail vocabulary."""
from __future__ import annotations

import logging
import os
from typing import Any

logger = logging.getLogger("aeon_observability")

# Argus's routing attributes, read from its collector config (platform/collector/agent.yaml) and not from
# its README, which does not state them. Its routing connector's condition, verbatim:
#
#   attributes["argus.hot"] == true
#     or status.code == STATUS_CODE_ERROR
#     or attributes["argus.guardrail"] != nil
#
# A span matching any of those reaches the hot path — an alert bus in about two seconds, skipping the
# datastore — as well as the cold one. Everything else waits 30-60s for ClickHouse.
ARGUS_HOT = "argus.hot"
ARGUS_GUARDRAIL = "argus.guardrail"

# The one guardrail the Python side owns. Policy denials, fan-out and approvals are refused in the Go
# gateways and marked there (go/internal/tracing/argus.go); a budget running out is decided in the
# workflow, so nothing else can report it.
GUARDRAIL_BUDGET_EXHAUSTED = "budget_exhausted"

# ARGUS'S CONVENTION: applications export to localhost, never to the central plane. A per-machine agent
# collector receives there and forwards. So this default is localhost and not the compose service name —
# pointing elsewhere is an explicit deployment decision, which is the whole point of the convention: the
# app's configuration stops changing when the infrastructure does.
DEFAULT_OTLP_ENDPOINT = "http://localhost:4318"

# A DEPLOYMENT FACT WORTH THE PARAGRAPH, because the naive reading of Argus's convention fails silently.
#
# The Argus agent binds to 127.0.0.1 only. Measured against the real agent running on this machine:
#
#   from a container on a docker network:  http://localhost:4318/v1/traces            -> 000 (unreachable)
#                                          http://host.docker.internal:4318/v1/traces -> 200
#
# So "applications export to localhost" is true of a process on the host and false of one in a container,
# and the failure mode is the worst kind: the exporter retries in the background, the app works perfectly,
# and no span ever arrives. Anything containerised must point at the host — host.docker.internal on Docker
# Desktop, or the gateway's address when the agent is not reachable at all.
#
# This is why init_tracing LOGS the endpoint it resolved. An operator who sees "exporting spans to
# http://localhost:4318" from inside a container has the answer in front of them.

# THE SAME ENV VAR THE GO SIDE READS. The first version of this module invented AEON_OTLP_ENDPOINT, which
# would have meant two names for one endpoint and a deployment that configured only one of them — half the
# services exporting and half silent, with nothing failing. Go's convention is host:port with no scheme
# (go/internal/tracing), so a value without one is accepted and http:// is added.
OTLP_ENDPOINT_ENV = "AEON_OTEL_ENDPOINT"


def _resolve_endpoint(explicit: str | None) -> str:
    raw = explicit or os.environ.get(OTLP_ENDPOINT_ENV) or DEFAULT_OTLP_ENDPOINT
    if "://" not in raw:
        raw = "http://" + raw
    return raw.rstrip("/")

_tracer: Any = None
_provider: Any = None


def init_tracing(service_name: str, endpoint: str | None = None) -> Any:
    """Configure the process-global tracer provider, or return a no-op one.

    IT NEVER RAISES, and that is deliberate: a worker that refuses to start because a collector is
    unreachable trades a diagnostic for an outage. Observability failing closed would make the platform
    less available than not having it. The failure is logged and the no-op tracer keeps every call site
    working unchanged.
    """
    global _tracer, _provider
    if _tracer is not None:
        return _tracer

    endpoint = _resolve_endpoint(endpoint)
    try:
        from opentelemetry import trace
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
        from opentelemetry.propagate import set_global_textmap
        from opentelemetry.propagators.composite import CompositePropagator
        from opentelemetry.sdk.resources import Resource
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import BatchSpanProcessor
        from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator
        from opentelemetry.baggage.propagation import W3CBaggagePropagator
    except ImportError as exc:
        logger.warning("aeon_observability: OpenTelemetry not installed (%s) — tracing disabled", exc)
        _tracer = _NoopTracer()
        return _tracer

    try:
        provider = TracerProvider(resource=Resource.create({"service.name": service_name}))
        provider.add_span_processor(BatchSpanProcessor(
            OTLPSpanExporter(endpoint=endpoint + "/v1/traces")
        ))
        trace.set_tracer_provider(provider)
        # The same composite the Go side sets, and it has to be the same: W3C traceparent is what carries
        # the trace across the HTTP hop into the gateways, and baggage is what would carry run_id if we
        # ever put it there.
        set_global_textmap(CompositePropagator([
            TraceContextTextMapPropagator(), W3CBaggagePropagator(),
        ]))
        _provider = provider
        _tracer = trace.get_tracer(service_name)
        logger.info("aeon_observability: exporting spans to %s as %s", endpoint, service_name)
    except Exception as exc:  # noqa: BLE001 - see the docstring: this must not take the worker down
        logger.warning("aeon_observability: could not initialise tracing against %s (%s) — disabled", endpoint, exc)
        _tracer = _NoopTracer()
    return _tracer


def tracer() -> Any:
    """The process tracer, initialising a no-op one if nobody called init_tracing."""
    if _tracer is None:
        return _NoopTracer()
    return _tracer


def shutdown() -> None:
    """Flush buffered spans. Called at worker shutdown and by tests that need to read spans back."""
    if _provider is not None:
        _provider.force_flush()
        _provider.shutdown()


def inject_trace_context(headers: dict[str, str]) -> dict[str, str]:
    """Add W3C traceparent/baggage to outgoing request headers.

    THIS IS THE LINE THAT MAKES ONE TRACE. Without it the gateway starts a root span and the run's spans
    are scattered across unrelated traces — each present, none connected, which is what OBS-001 actually
    shipped. Mutates and returns headers so a caller can write it inline at the request it belongs to.
    """
    try:
        from opentelemetry.propagate import inject

        inject(headers)
    except Exception as exc:  # noqa: BLE001
        logger.debug("aeon_observability: could not inject trace context (%s)", exc)
    return headers


def current_traceparent() -> str:
    """The current W3C traceparent, or "" when there is no recording span.

    Exposed for tests and for logs: a trace id in a log line is what lets someone move from a message to
    the trace, and reconstructing it by hand from the span context is the kind of thing each caller would
    do slightly differently.
    """
    headers: dict[str, str] = {}
    inject_trace_context(headers)
    return headers.get("traceparent", "")


def mark_guardrail(span: Any, kind: str, reason: str) -> None:
    """Record that a guardrail refused this operation, routing the span to Argus's hot path.

    It does NOT set the span status to error, for the same reason the Go side does not: a budget stop is
    the system working, and reporting it as an error buries real failures under a stream of correct
    refusals. Argus routes on the guardrail attribute independently of status, which is exactly why the
    attribute exists separately.
    """
    if span is None:
        return
    try:
        span.set_attribute(ARGUS_GUARDRAIL, kind)
        span.set_attribute("aeon.guardrail.reason", reason)
    except Exception as exc:  # noqa: BLE001
        logger.debug("aeon_observability: could not mark guardrail (%s)", exc)


class _NoopSpan:
    """Absorbs every span operation so call sites need no conditionals.

    The alternative — `if tracer: span = ...` at each call site — is how instrumentation ends up present in
    some paths and absent in others, and the absent ones are never the ones anybody notices.
    """

    def __enter__(self) -> "_NoopSpan":
        return self

    def __exit__(self, *_: object) -> None:
        return None

    def set_attribute(self, *_: object) -> None:
        return None

    def set_status(self, *_: object) -> None:
        return None

    def record_exception(self, *_: object) -> None:
        return None

    def end(self) -> None:
        return None


class _NoopTracer:
    def start_as_current_span(self, *_: object, **__: object) -> _NoopSpan:
        return _NoopSpan()

    def start_span(self, *_: object, **__: object) -> _NoopSpan:
        return _NoopSpan()
