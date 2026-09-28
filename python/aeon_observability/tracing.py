"""Telemetry for the Python side, on Argus's own SDK (OBS-006b).

WHY THE SDK AND NOT PLAIN OTLP, since the first version of this module argued the opposite. I claimed the
integration was "an endpoint plus two attribute names, so a dependency buys nothing". Reading their code
showed that is wrong in the way that matters: `argus-obs-semconv` carries the VALUES and the companion
attributes, not only the keys.

  - `argus.guardrail` is a COMMA-SEPARATED LIST of breached guardrails, and it arrives together with
    `argus.hot`. My hand-rolled version set a single string and deliberately did not set `argus.hot`.
  - Their guardrail kinds are hyphenated (`tool-call-budget`, `cost-budget`, `token-budget`,
    `tool-call-loop`). Mine were snake_case inventions.
  - `GenAISpan` and `Step` already model what Aeon spent three features naming: `Step.outcome()` is
    INT-011's outcome, `usage(input_tokens=, output_tokens=, cached_input_tokens=)` is MDL-014's
    three-state counters, and `backend(circuit_state=, fallback=, backend_id=)` is A5 and OBS-005.

Hand-rolled attributes would have produced telemetry that looks right and that their alert rules and
dashboards silently do not match — the most expensive kind of nearly-correct, and the exact failure mode
this project keeps finding in its own artefacts.

WHAT IS STILL OURS, and it is a boundary worth stating. `argus_semconv.guardrails` ships a Budget/AgentRun
model that tracks tool calls and cost in CONTEXTVARS. Aeon does not use it for enforcement: our budgets
live in Temporal workflow state because they must be deterministic and survive a replay, and contextvars
are neither. We use their VOCABULARY to report a breach and our own state to decide there was one.

This module stays as a thin seam over `argus` rather than letting call sites import it directly, for one
reason: every span Aeon emits should carry Aeon's run/step identity, and a seam is where that happens once
instead of at forty call sites.
"""
from __future__ import annotations

import logging
import os
from contextlib import contextmanager
from typing import Any, Iterator

logger = logging.getLogger("aeon_observability")

# Argus's routing attributes, for the places Aeon sets them directly (the Go side mirrors these in
# go/internal/tracing/argus.go). Read from platform/collector/agent.yaml, whose routing connector is the
# only place they are stated: a span reaches the hot path — an alert bus in ~2s, skipping the datastore —
# when `argus.hot` is true, the status is ERROR, or `argus.guardrail` is present.
ARGUS_HOT = "argus.hot"
ARGUS_GUARDRAIL = "argus.guardrail"

# Guardrail kinds. The first four are THEIRS, verbatim from argus_semconv.guardrails, so a breach Aeon
# reports lands in the same bucket as one their own Budget model would report. The rest are Aeon's, in
# their hyphenated style, for refusals their model has no concept of — a Cedar policy denial is not a
# budget, and calling it `cost-budget` to reuse a name would make two different incidents look identical.
GUARDRAIL_TOOL_CALL_BUDGET = "tool-call-budget"
GUARDRAIL_TOOL_CALL_LOOP = "tool-call-loop"
GUARDRAIL_TOKEN_BUDGET = "token-budget"
GUARDRAIL_COST_BUDGET = "cost-budget"
GUARDRAIL_POLICY_DENIED = "policy-denied"
GUARDRAIL_APPROVAL_REQUIRED = "approval-required"
GUARDRAIL_FAN_OUT = "fan-out-budget"
GUARDRAIL_DESTINATION_UNKNOWN = "destination-not-declared"

_handle: Any = None
_argus: Any = None


def init_tracing(service_name: str | None = None, endpoint: str | None = None) -> Any:
    """Initialise Argus telemetry. Never raises.

    Their `init()` is documented as idempotent and failure-proof, which is the same property this module
    needed anyway: a worker that refuses to start because a collector is unreachable trades a diagnostic
    for an outage. The import is still guarded, because a deployment that has not installed the extra
    should degrade to no telemetry rather than to no worker.

    ENV VARS ARE THEIRS: `ARGUS_SERVICE`, `ARGUS_ENDPOINT` (default localhost:4317/4318),
    `ARGUS_ENVIRONMENT`, `ARGUS_ROLE`, `ARGUS_DISABLED`, with the `OTEL_*` equivalents as fallbacks. Aeon
    passes AEON_SERVICE_NAME through when set, so the compose file keeps naming services the way the rest
    of this repo does, and otherwise stays out of the way.
    """
    global _handle, _argus
    if _handle is not None:
        return _handle
    try:
        import argus
    except ImportError as exc:
        logger.warning("aeon_observability: argus-obs-sdk not installed (%s) — telemetry disabled", exc)
        _argus = None
        _handle = _NoopHandle()
        return _handle

    _argus = argus
    kwargs: dict[str, Any] = {}
    service = service_name or os.environ.get("AEON_SERVICE_NAME")
    if service:
        kwargs["service"] = service

    # AEON_OTEL_ENDPOINT is bridged to their `endpoint` argument, and the bridge is not cosmetic.
    #
    # The Go services read AEON_OTEL_ENDPOINT and the compose file sets it; their SDK reads ARGUS_ENDPOINT or
    # OTEL_EXPORTER_OTLP_ENDPOINT. Without this, moving to the SDK would have left the worker silently
    # exporting to its DEFAULT (localhost) while the compose file said otherwise — the endpoint configured in
    # one place and read from another, with nothing failing. Their variables still win when set, because a
    # deployment that speaks Argus's own configuration should not have to learn ours.
    resolved = (
        endpoint
        or os.environ.get("ARGUS_ENDPOINT")
        or os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT")
        or os.environ.get("AEON_OTEL_ENDPOINT")
    )
    if resolved:
        kwargs["endpoint"] = resolved
    try:
        _handle = argus.init(**kwargs)
        logger.info(
            "aeon_observability: argus %s initialised (service=%s, endpoint=%s)",
            getattr(argus, "__version__", "?"),
            service or os.environ.get("ARGUS_SERVICE") or "<from env>",
            resolved or "<their default: localhost>",
        )
    except Exception as exc:  # noqa: BLE001 - see the docstring
        logger.warning("aeon_observability: argus.init failed (%s) — telemetry disabled", exc)
        _handle = _NoopHandle()
    return _handle


def inject_trace_context(headers: dict[str, str]) -> dict[str, str]:
    """Add the propagation headers to an outgoing request.

    Delegates to `argus.propagate.inject_headers`, which is the SDK's own and therefore agrees with
    whatever trust mode and header set Argus expects — `ARGUS_PROPAGATE` governs the inbound side, and
    hand-writing the outbound half is how the two drift.

    THIS IS THE LINE THAT MAKES ONE TRACE. Without it a gateway starts a root span and a run's spans end up
    scattered across unrelated traces, each present and none connected.
    """
    if _argus is None:
        return headers
    try:
        headers.update(_argus.propagate.inject_headers())
    except Exception as exc:  # noqa: BLE001
        logger.debug("aeon_observability: could not inject trace context (%s)", exc)
    return headers


def current_traceparent() -> str:
    """The current W3C traceparent, or "" when nothing is recording."""
    return inject_trace_context({}).get("traceparent", "")


@contextmanager
def tool_span(name: str, *, args: Any = None, call_id: str | None = None) -> Iterator[Any]:
    """An `execute_tool` span, via `argus.tool`.

    `args` goes to the SDK, which decides whether to record it: content capture is off unless
    `ARGUS_CAPTURE_CONTENT` is set, and that decision belongs to the platform operator rather than to
    this call site. Passing the arguments and letting their masking apply is the difference between
    honouring that switch and quietly ignoring it.
    """
    if _argus is None:
        yield _NoopSpan()
        return
    with _argus.tool(name, args=args, call_id=call_id) as span:
        yield span


@contextmanager
def chat_span(*, provider: str, request_model: str | None = None) -> Iterator[Any]:
    """A `chat` span, via `argus.genai` with the operation literal their semconv defines."""
    if _argus is None:
        yield _NoopSpan()
        return
    with _argus.genai("chat", provider=provider, request_model=request_model) as span:
        yield span


@contextmanager
def run_span(name: str, *, run_id: str | None = None) -> Iterator[Any]:
    """A run-scoped span, via `argus.propagate.run`.

    Their helper puts the run id in BAGGAGE as well as on the span, which is what carries it across the
    HTTP hop into the Go gateways — something a plain span attribute cannot do.
    """
    if _argus is None:
        yield _NoopSpan()
        return
    with _argus.propagate.run(name, run_id=run_id) as span:
        yield span


@contextmanager
def step_span(name: str, **fields: Any) -> Iterator[Any]:
    """An Aeon durable step, via `argus.step`.

    Used where INT-011's outcome vocabulary belongs: the returned `Step` has `.outcome(value)`, which is
    the same concept `checkpoint.Outcome` names on the Go side. Reporting our outcome through their field
    means a step that was denied, approved or expired is queryable in their store under the name their
    tooling already uses.
    """
    if _argus is None:
        yield _NoopSpan()
        return
    with _argus.step(name, **fields) as step:
        yield step


def mark_guardrail(span: Any, kind: str, reason: str) -> None:
    """Record a guardrail breach on a span, in Argus's own shape.

    BOTH attributes, because that is what their own `AgentRun.attributes()` emits: `argus.guardrail` with
    the kind and `argus.hot` true. My first version set only the guardrail and argued `argus.hot` was
    redundant since the router triggers on either — true of the router, and wrong for everything
    downstream that filters on `argus.hot` because their SDK always sets it.

    It still does NOT set the span status to error: a policy denial is the system working, and reporting it
    as an error would bury real failures under a stream of correct refusals.
    """
    if span is None:
        return
    target = getattr(span, "span", span)
    try:
        target.set_attribute(ARGUS_GUARDRAIL, kind)
        target.set_attribute(ARGUS_HOT, True)
        target.set_attribute("aeon.guardrail.reason", reason)
    except Exception as exc:  # noqa: BLE001
        logger.debug("aeon_observability: could not mark guardrail (%s)", exc)


def shutdown() -> None:
    """Flush buffered telemetry. For worker shutdown and for tests that read spans back."""
    if _handle is None:
        return
    for attr in ("shutdown", "flush", "force_flush"):
        fn = getattr(_handle, attr, None)
        if callable(fn):
            try:
                fn()
                return
            except Exception as exc:  # noqa: BLE001
                logger.debug("aeon_observability: %s failed (%s)", attr, exc)


class _NoopSpan:
    """Absorbs every operation so call sites need no conditionals.

    The alternative — `if telemetry: ...` at each site — is how instrumentation ends up present in some
    paths and missing in others, and the missing ones are never the ones anybody notices.
    """

    span: Any = None

    def __enter__(self) -> "_NoopSpan":
        return self

    def __exit__(self, *_: object) -> None:
        return None

    def __getattr__(self, _name: str) -> Any:
        return lambda *a, **k: self


class _NoopHandle:
    def __getattr__(self, _name: str) -> Any:
        return lambda *a, **k: None
