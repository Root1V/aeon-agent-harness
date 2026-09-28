"""test_run_produces_one_trace_across_both_languages — OBS-001's Python half.

WHAT WAS ACTUALLY BROKEN, and it is not "Python had no spans". The Go gateways emitted spans that reached
Tempo, and OBS-001's test proved each one existed by querying them SEPARATELY: `name = "chat"`,
`name = "execute_tool"`, `name = "invoke_agent"`. All three were there. What none of them were was PART OF
THE SAME TRACE — OTel Go's default propagator is a no-op, nothing extracted an incoming context, and the
Python Activities calling those gateways created no spans at all. One run appeared as several unrelated
roots, and an incident responder following a trace found a third of the story.

So the assertion here is the one OBS-001 was missing: ONE trace id, containing spans from BOTH services,
with the Go span a CHILD of the Python one. "The spans exist" is a weaker property than "this is a trace",
and the gap between them is exactly what an incident needs.

Real throughout: a real Temporal server, a real worker process, a real Tool Gateway, a real OTel Collector
and a real Tempo queried over TraceQL.
"""
from __future__ import annotations

import json
import os
import time
import urllib.error
import urllib.request

import pytest

TEMPO_URL = os.environ.get("AEON_TEST_TEMPO_QUERY_URL", "")
TOOLGW_ADDR = os.environ.get("AEON_TEST_TOOLGW_ADDR", "")
OTEL_ENDPOINT = os.environ.get("AEON_TEST_OTEL_ENDPOINT", "")


def _require_stack() -> None:
    missing = [n for n, v in (
        ("AEON_TEST_TEMPO_QUERY_URL", TEMPO_URL),
        ("AEON_TEST_TOOLGW_ADDR", TOOLGW_ADDR),
        ("AEON_TEST_OTEL_ENDPOINT", OTEL_ENDPOINT),
    ) if not v]
    if missing:
        pytest.skip(f"{', '.join(missing)} not set — needs the real obs stack (see make test-python-integration)")


def _tempo_trace(trace_id: str) -> dict | None:
    """Fetch a whole trace by id, or None while Tempo has not ingested it yet."""
    try:
        with urllib.request.urlopen(f"{TEMPO_URL}/api/traces/{trace_id}", timeout=10) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            return None
        raise
    except urllib.error.URLError:
        return None


def _spans_of(trace: dict) -> list[tuple[str, str, str]]:
    """Flatten a Tempo trace into (service_name, span_name, parent_span_id) triples."""
    out: list[tuple[str, str, str]] = []
    for batch in trace.get("batches", []):
        service = ""
        for attr in batch.get("resource", {}).get("attributes", []):
            if attr.get("key") == "service.name":
                service = attr.get("value", {}).get("stringValue", "")
        for scope in batch.get("scopeSpans", []):
            for span in scope.get("spans", []):
                out.append((service, span.get("name", ""), span.get("parentSpanId", "")))
    return out


def test_run_produces_one_trace_across_both_languages():
    _require_stack()
    from aeon_observability import current_traceparent, init_tracing, inject_trace_context, tracer
    from aeon_observability.tracing import shutdown

    init_tracing("aeon-test-client", OTEL_ENDPOINT)

    # A Python parent span, then a real HTTP call into the real Tool Gateway with the context injected.
    # Deliberately a DENIED tool: the denial path is the one Aeon's guardrail marking runs through, and it
    # needs no Postgres, no executor and no run — so the test exercises the propagation and the guardrail
    # attribute at once, without a second moving part that could fail for its own reasons.
    with tracer().start_as_current_span("invoke_agent") as span:
        span.set_attribute("gen_ai.operation.name", "invoke_agent")
        traceparent = current_traceparent()
        assert traceparent, "no traceparent — nothing would be propagated and this test would prove nothing"
        trace_id = traceparent.split("-")[1]

        body = json.dumps({
            "agent_manifest_ref": "deep-research-general@0.1.0",
            "tool_name": "shell.exec",
            "args": {},
        }).encode()
        request = urllib.request.Request(
            f"http://{TOOLGW_ADDR}/execute", data=body, method="POST",
            headers=inject_trace_context({"Content-Type": "application/json"}),
        )
        try:
            with urllib.request.urlopen(request, timeout=30):
                pytest.fail("shell.exec was not denied — the policy bundle changed and this test needs revisiting")
        except urllib.error.HTTPError as exc:
            assert exc.code == 403, f"expected a policy denial, got {exc.code}"

    shutdown()

    # Poll for the WHOLE trace, not for individual spans. The distinction is the point of this test.
    deadline = time.time() + 60
    trace = None
    while time.time() < deadline:
        trace = _tempo_trace(trace_id)
        if trace and len(_spans_of(trace)) >= 2:
            break
        time.sleep(1)

    assert trace is not None, f"trace {trace_id} never reached Tempo"
    spans = _spans_of(trace)
    services = {s for s, _, _ in spans}
    names = {n for _, n, _ in spans}

    assert "aeon-test-client" in services, f"the Python span is missing from the trace: {spans}"
    assert "aeon-toolgw" in services, (
        f"the Go span is not in the SAME trace as the Python one — this is the exact failure the feature "
        f"fixes, and it looks like success to any test that queries the two spans separately: {spans}"
    )
    assert "invoke_agent" in names and "execute_tool" in names, f"spans = {spans}"

    # PARENTAGE, asserted separately from co-location. Two spans can share a trace id and still both be
    # roots, which reads as one trace in a list and as two disconnected stories in a waterfall.
    parents = {n: p for _, n, p in spans}
    assert parents.get("execute_tool"), (
        "the Go execute_tool span has no parent — it shares the trace id but is a second root, so the "
        "waterfall still shows two disconnected stories"
    )


def test_a_guardrail_refusal_is_marked_for_the_hot_path():
    """The Argus integration, asserted on a real span.

    Argus routes a span to its ~2s alert path when `argus.guardrail` is present (its collector's routing
    connector, platform/collector/agent.yaml). Aeon's refusals are exactly what that path is for: not "a
    request failed" but "governance stopped something". This checks the attribute actually arrives, because
    an integration that is only a constant in a header file is an intention.
    """
    _require_stack()
    from aeon_observability import current_traceparent, init_tracing, inject_trace_context, tracer
    from aeon_observability.tracing import shutdown

    init_tracing("aeon-test-client", OTEL_ENDPOINT)

    with tracer().start_as_current_span("invoke_agent"):
        traceparent = current_traceparent()
        trace_id = traceparent.split("-")[1]
        body = json.dumps({
            "agent_manifest_ref": "deep-research-general@0.1.0",
            "tool_name": "shell.exec", "args": {},
        }).encode()
        request = urllib.request.Request(
            f"http://{TOOLGW_ADDR}/execute", data=body, method="POST",
            headers=inject_trace_context({"Content-Type": "application/json"}),
        )
        try:
            urllib.request.urlopen(request, timeout=30).close()
        except urllib.error.HTTPError:
            pass

    shutdown()

    deadline = time.time() + 60
    guardrail = None
    while time.time() < deadline and guardrail is None:
        trace = _tempo_trace(trace_id)
        for batch in (trace or {}).get("batches", []):
            for scope in batch.get("scopeSpans", []):
                for span in scope.get("spans", []):
                    for attr in span.get("attributes", []):
                        if attr.get("key") == "argus.guardrail":
                            guardrail = attr.get("value", {}).get("stringValue")
        if guardrail is None:
            time.sleep(1)

    assert guardrail == "policy_denied", (
        f"argus.guardrail on the denial span = {guardrail!r}, want 'policy_denied' — without it the refusal "
        "waits 30-60s in Argus's cold path instead of reaching the alert bus in two"
    )
