package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"

	"github.com/aeon-ai/aeon/go/internal/httpserver"
	"github.com/aeon-ai/aeon/go/internal/runcontroller"
	"github.com/aeon-ai/aeon/go/internal/tracing"
)

// TestRunStartedOverHTTPIsOneTraceDownToTheWorker is OBS-010b's acceptance test.
//
// WHAT WAS BROKEN, and Argus found it from their own store before we did. `OBS-010` installed
// Temporal's OTel interceptor on the Python side, which made a run's Activity spans one trace instead
// of eight unrelated roots. The first hop stayed loose: `aeon-runcontroller` dialled Temporal with no
// interceptors, so the trace context of the HTTP request that starts a run never entered the
// workflow's history, and the worker's trace began at the worker. Their report was exact — "lo que
// todavía no veo es una traza que cruce del runcontroller al worker" — and there was nothing to see.
//
// WHY THIS ASSERTION AND NOT "THE SPANS EXIST". OBS-001's test queried each span separately by name,
// all three were found, and the conclusion it drew was false: they were three unrelated roots. That
// mistake is the reason this test fetches the WHOLE trace by the id the HTTP client generated and
// asserts on its contents — co-location AND parentage, in two separate assertions, because two spans
// can share a trace id and both still be roots, which reads as one trace in a list and as two
// disconnected stories in a waterfall.
//
// Real throughout: a real Temporal server, the real Python worker from the compose stack executing a
// real GraphRunWorkflow, the real Run Controller handler over HTTP, a real OTel Collector and a real
// Tempo read back over its API.
func TestRunStartedOverHTTPIsOneTraceDownToTheWorker(t *testing.T) {
	tempoURL := os.Getenv("AEON_TEST_TEMPO_QUERY_URL")
	if tempoURL == "" {
		t.Skip("AEON_TEST_TEMPO_QUERY_URL not set — skipping (see make test-go-integration)")
	}
	flush := ensureTestTracing(t)

	addr := os.Getenv("AEON_TEST_TEMPORAL_ADDRESS")
	if addr == "" {
		t.Skip("AEON_TEST_TEMPORAL_ADDRESS not set — skipping (see make test-go-integration)")
	}
	// THE THING UNDER TEST is on this line. A client dialled without it produces every span this test
	// looks for, in two separate traces.
	c, err := client.Dial(client.Options{
		HostPort:     addr,
		Interceptors: []interceptor.ClientInterceptor{tracing.TemporalInterceptor()},
	})
	if err != nil {
		t.Fatalf("connecting to Temporal at %s: %v", addr, err)
	}
	t.Cleanup(c.Close)

	mux := http.NewServeMux()
	(&RunControllerHandlers{Controller: runcontroller.New(c, "")}).Register(mux)
	// ExtractTraceContext, the same wrapper all four binaries mount (OBS-006b). Without it the inbound
	// traceparent below is ignored and the handler starts a root span of its own — so this test would
	// be checking propagation while skipping the step that receives it.
	srv := httptest.NewServer(httpserver.ExtractTraceContext(mux))
	t.Cleanup(srv.Close)

	// A traceparent written by hand rather than taken from a span we started. This is what an incident
	// responder follows: an id that existed BEFORE this process was involved. Version 00, sampled.
	traceID := fmt.Sprintf("%032x", time.Now().UnixNano())
	traceparent := "00-" + traceID + "-00f067aa0ba902b7-01"

	runID := newRunID("obs010b")
	body, err := json.Marshal(map[string]any{
		"run_id": runID,
		"graph":  simpleGraph("obs010b-" + runID + ".txt"),
	})
	if err != nil {
		t.Fatalf("marshalling the start request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/runs", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building the start request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", traceparent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /runs: status=%d body=%s", resp.StatusCode, raw)
	}

	// The real worker has to actually run it: the spans this test is about are the worker's.
	waitForStatus(t, srv, runID, "SUCCEEDED", 60*time.Second)
	if err := flush(t.Context()); err != nil {
		t.Fatalf("flushing spans: %v", err)
	}

	// Polled until TWO SERVICES are in the trace, not until N spans are. The worker alone puts five
	// spans in it within a second, so a span count is satisfied before the Go side's own span has been
	// ingested — the first version of this test failed for exactly that reason and the failure looked
	// like a missing span rather than an impatient poll.
	trace := waitForWholeTrace(t, tempoURL, traceID, 2, 90*time.Second)
	spans := flattenTrace(trace)
	if len(spans) == 0 {
		t.Fatalf("trace %s reached Tempo with no spans", traceID)
	}

	services := map[string]bool{}
	parents := map[string]string{}
	for _, s := range spans {
		services[s.service] = true
		parents[s.name] = s.parent
	}
	t.Logf("trace %s: %d span(s) across %d service(s): %v", traceID, len(spans), len(services), spans)

	if !services["aeon-api-integration-test"] {
		t.Fatalf("the Go side's own span is missing from the trace: %v", spans)
	}
	// The Python worker, under whatever service name the compose stack gives it. Matched on the
	// service rather than a span name because the worker's span names come from its own
	// instrumentation, and this assertion is about WHOSE spans are in the trace.
	var workerService string
	for s := range services {
		if s != "aeon-api-integration-test" {
			workerService = s
		}
	}
	if workerService == "" {
		t.Fatalf("no second service in trace %s — the worker's spans are in a trace of their own, which "+
			"is the exact failure this feature fixes and looks like success to any query that asks for "+
			"the spans one at a time: %v", traceID, spans)
	}

	// The INBOUND span id is the parent of something, which is what proves the header was honoured
	// rather than a trace that merely carries the same id. "00f067aa0ba902b7" belongs to a caller that
	// is not in this trace at all — exactly the shape of a request arriving from another service.
	inboundParent := base64.StdEncoding.EncodeToString([]byte{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7})
	foundInbound := false
	for _, s := range spans {
		if s.parent == inboundParent {
			foundInbound = true
		}
	}
	if !foundInbound {
		t.Fatalf("no span in trace %s descends from the inbound traceparent's span id — the header was "+
			"accepted for the trace id and its parentage dropped: %v", traceID, spans)
	}

	// PARENTAGE, asserted separately. Every span in the trace must have a parent except the one the
	// inbound traceparent named — a second parentless span means a second root, and a waterfall with
	// two roots tells two disconnected stories however many spans share the id.
	var roots []string
	for _, s := range spans {
		if s.parent == "" {
			roots = append(roots, s.service+"/"+s.name)
		}
	}
	if len(roots) > 0 {
		t.Fatalf("trace %s has %d parentless span(s) %v — the inbound traceparent named the root, so "+
			"every span here should descend from it", traceID, len(roots), roots)
	}
}

// TestTemporalAndHTTPHopsCarryTheSameFormats pins the coincidence OBS-010b relies on.
//
// Their `TracerOptions.TextMapPropagator` defaults to their OWN composite and explicitly not to the
// OpenTelemetry global one, which is the propagator the HTTP hop uses. The two happen to carry the
// same formats today, which is why tracing.TemporalInterceptor passes nothing — see the reasoning
// there for why passing the global would be worse. A coincidence of two libraries' defaults is not a
// contract, so it is asserted: if either side adds or drops a format, a run would propagate across
// one hop and not the other, and nothing else in this repo would notice.
func TestTemporalAndHTTPHopsCarryTheSameFormats(t *testing.T) {
	httpFields := map[string]bool{}
	for _, f := range tracing.Propagator().Fields() {
		httpFields[f] = true
	}
	temporalFields := map[string]bool{}
	for _, f := range tracing.TemporalPropagator().Fields() {
		temporalFields[f] = true
	}
	if len(httpFields) == 0 {
		t.Fatalf("the HTTP propagator carries no fields at all — Go's global default is a no-op, and a "+
			"no-op here means nothing propagates over HTTP: %v", tracing.Propagator())
	}
	for f := range httpFields {
		if !temporalFields[f] {
			t.Fatalf("the HTTP hop carries %q and the Temporal hop does not (%v vs %v) — a run would "+
				"propagate across one hop and break at the other", f, httpFields, temporalFields)
		}
	}
	for f := range temporalFields {
		if !httpFields[f] {
			t.Fatalf("the Temporal hop carries %q and the HTTP hop does not (%v vs %v)", f, temporalFields, httpFields)
		}
	}
}

// tempoSpan is one span of a fetched trace, flattened to what these assertions are about.
type tempoSpan struct {
	service string
	name    string
	parent  string
}

func (s tempoSpan) String() string { return s.service + "/" + s.name + "(parent=" + s.parent + ")" }

// waitForWholeTrace fetches a whole trace by id, polling until it holds at least minSpans.
//
// BY ID AND NOT BY SEARCH, which is the distinction OBS-001 got wrong: a TraceQL search that finds a
// span proves the span exists, and this test is about whether the spans are one trace.
func waitForWholeTrace(t *testing.T, tempoURL, traceID string, minServices int, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		resp, err := http.Get(tempoURL + "/api/traces/" + traceID)
		if err == nil {
			raw, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK {
				var parsed map[string]any
				if json.Unmarshal(raw, &parsed) == nil {
					last = parsed
					if distinctServices(flattenTrace(parsed)) >= minServices {
						return parsed
					}
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("trace %s never reached Tempo with spans from %d or more services (last: %v)", traceID, minServices, flattenTrace(last))
	return nil
}

// distinctServices counts how many services contributed spans to a trace.
func distinctServices(spans []tempoSpan) int {
	seen := map[string]bool{}
	for _, s := range spans {
		seen[s.service] = true
	}
	return len(seen)
}

// flattenTrace reads Tempo's OTLP-JSON trace into (service, name, parentSpanId) triples.
func flattenTrace(trace map[string]any) []tempoSpan {
	var out []tempoSpan
	batches, _ := trace["batches"].([]any)
	for _, b := range batches {
		batch, _ := b.(map[string]any)
		service := ""
		if res, ok := batch["resource"].(map[string]any); ok {
			attrs, _ := res["attributes"].([]any)
			for _, a := range attrs {
				attr, _ := a.(map[string]any)
				if attr["key"] == "service.name" {
					if v, ok := attr["value"].(map[string]any); ok {
						service, _ = v["stringValue"].(string)
					}
				}
			}
		}
		scopes, _ := batch["scopeSpans"].([]any)
		for _, sc := range scopes {
			scope, _ := sc.(map[string]any)
			spans, _ := scope["spans"].([]any)
			for _, sp := range spans {
				span, _ := sp.(map[string]any)
				name, _ := span["name"].(string)
				parent, _ := span["parentSpanId"].(string)
				out = append(out, tempoSpan{service: service, name: name, parent: parent})
			}
		}
	}
	return out
}
