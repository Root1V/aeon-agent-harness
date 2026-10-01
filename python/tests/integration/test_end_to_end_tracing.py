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


def _gateway_headers() -> dict[str, str]:
    """SEC-005: the Tool Gateway authenticates now, so this test has to be somebody.

    It presents the development bundle's `integration-test` caller, which is declared with
    `mayActAs: [deep-research-general@0.1.0]` — the agent both tests below claim. A test that sent no
    credential would get a 401 where it asserts a 403, and the denial it is about (Cedar refusing
    shell.exec) would never be reached: the assertion would still read as "the call was refused".
    """
    from aeon_observability import inject_trace_context

    token = os.environ.get("AEON_CALLER_TOKEN", "dev-test-token-not-a-secret")
    return inject_trace_context({"Content-Type": "application/json", "Authorization": "Bearer " + token})


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
    from aeon_observability import current_traceparent, flush, init_tracing, run_span

    # The SDK decides its protocol from the installed exporters, and this image has both — so it must be
    # pinned, or it speaks gRPC at an HTTP port and every export fails in a retry loop. Found by running it.
    os.environ.setdefault("ARGUS_PROTOCOL", "http/protobuf")
    init_tracing("aeon-test-client", "http://" + OTEL_ENDPOINT)

    # A Python parent span, then a real HTTP call into the real Tool Gateway with the context injected.
    # Deliberately a DENIED tool: the denial path is the one Aeon's guardrail marking runs through, and it
    # needs no Postgres, no executor and no run — so the test exercises the propagation and the guardrail
    # attribute at once, without a second moving part that could fail for its own reasons.
    # argus.propagate.run(...) via the seam: their run helper, which also puts the run id in BAGGAGE —
    # the thing a plain span attribute cannot carry across the HTTP hop into the Go gateways.
    with run_span("invoke_agent", run_id="tracing-test") as span:
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
            headers=_gateway_headers(),
        )
        try:
            with urllib.request.urlopen(request, timeout=30):
                pytest.fail("shell.exec was not denied — the policy bundle changed and this test needs revisiting")
        except urllib.error.HTTPError as exc:
            assert exc.code == 403, f"expected a policy denial, got {exc.code}"

    flush()

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
    # `toolgw` and not `aeon-toolgw`: the component name dropped the `aeon-` prefix when Argus put us in
    # their registry (2026-09-29) — with service.namespace=aeon-ai present, the prefix repeated what the
    # namespace already says. This assertion still named the old one, and nothing noticed until the whole
    # integration target ran: it is the only place that runs this file, and it had not run since.
    assert "toolgw" in services, (
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


def test_a_policy_denial_is_recorded_as_denied_and_does_not_page():
    """The Argus integration, asserted on a real span, after their correction.

    Argus routes a span to its ~2s alert path when `argus.guardrail` is present (its collector's routing
    connector, platform/collector/agent.yaml). Aeon's refusals are exactly what that path is for: not "a
    request failed" but "governance stopped something". This checks the attribute actually arrives, because
    an integration that is only a constant in a header file is an intention.
    """
    _require_stack()
    from aeon_observability import current_traceparent, flush, init_tracing, run_span

    # The SDK decides its protocol from the installed exporters, and this image has both — so it must be
    # pinned, or it speaks gRPC at an HTTP port and every export fails in a retry loop. Found by running it.
    os.environ.setdefault("ARGUS_PROTOCOL", "http/protobuf")
    init_tracing("aeon-test-client", "http://" + OTEL_ENDPOINT)

    with run_span("invoke_agent", run_id="guardrail-test"):
        traceparent = current_traceparent()
        trace_id = traceparent.split("-")[1]
        body = json.dumps({
            "agent_manifest_ref": "deep-research-general@0.1.0",
            "tool_name": "shell.exec", "args": {},
        }).encode()
        request = urllib.request.Request(
            f"http://{TOOLGW_ADDR}/execute", data=body, method="POST",
            headers=_gateway_headers(),
        )
        try:
            urllib.request.urlopen(request, timeout=30).close()
        except urllib.error.HTTPError:
            pass

    flush()

    deadline = time.time() + 60
    guardrail, outcome = None, None
    while time.time() < deadline and outcome is None:
        trace = _tempo_trace(trace_id)
        for batch in (trace or {}).get("batches", []):
            for scope in batch.get("scopeSpans", []):
                for span in scope.get("spans", []):
                    for attr in span.get("attributes", []):
                        if attr.get("key") == "argus.guardrail":
                            guardrail = attr.get("value", {}).get("stringValue")
                        if attr.get("key") == "argus.outcome":
                            outcome = attr.get("value", {}).get("stringValue")
        if outcome is None:
            time.sleep(1)

    # NO GUARDRAIL on a policy denial any more, on Argus's recommendation of 2026-09-29: setting
    # argus.guardrail requests a page within two seconds, and a Cedar denial may be routine in a given
    # deployment. The refusal is carried by argus.outcome=denied, which their gateway's audit policy
    # retains in full — they measured it: with only outcome=denied, sampling kept 0 of 8 denials until
    # `denied` entered that policy, then 8 of 8.
    #
    # `denied` and not `ok`, which is where they corrected us: a denial counted as `ok` is counted with the
    # successes, and "how many times did policy say no" becomes unanswerable.
    assert outcome == "denied", (
        f"argus.outcome on the denial span = {outcome!r}, want 'denied'. Their Step finaliser defaults this "
        "to 'ok', so a refusal that set nothing would be stored as a successful step"
    )
    assert guardrail is None, (
        f"argus.guardrail = {guardrail!r} on a policy denial. Its presence alone requests a two-second "
        "page, and a routine denial paging every time is alert fatigue built from inside"
    )
