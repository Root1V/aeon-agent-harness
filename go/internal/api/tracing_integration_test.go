package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
)

// tracingFakeProvider is a minimal providers.Provider test double, mirroring
// modelgateway.fakeProvider — duplicated here rather than imported since that one is unexported
// test-only code in a different package.
//
// The duplication has already cost something, and the note is here so the next person pays it once.
// MDL-015 made the gateway treat a response with no content and no tool calls as the candidate FAILING;
// modelgateway.fakeProvider was updated for it and this copy was not, so Decide below returned a bare
// {"model": ...} and this test broke. It went unnoticed because this file only runs under the obs
// compose profile, so the suite looked green while a double in it had stopped resembling a provider.
type tracingFakeProvider struct{}

func (tracingFakeProvider) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	// A USABLE normalized response: this test needs the candidate to SUCCEED so a "chat" span with
	// gen_ai.request.model reaches Tempo. The shape mirrors what a real provider adapter normalizes to.
	return map[string]any{
		"model": renderedContext["model"],
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "ok"},
		}},
	}, nil
}
func (tracingFakeProvider) CachingCapability() string { return "none" }
func (tracingFakeProvider) CostModel() string         { return "token_based" }

// tempoSearch is the subset of Tempo's GET /api/search response this test needs.
type tempoSearch struct {
	Traces []struct {
		TraceID string `json:"traceID"`
	} `json:"traces"`
}

// waitForTempoSpan polls Tempo's TraceQL search API until traceQL matches at least one trace, or
// fails the test after timeout. tempoURL is the base query URL (e.g. "http://tempo:3200").
func waitForTempoSpan(t *testing.T, tempoURL, traceQL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	reqURL := tempoURL + "/api/search?" + url.Values{"q": {traceQL}, "limit": {"5"}}.Encode()

	var lastErr error
	var lastBody tempoSearch
	for time.Now().Before(deadline) {
		resp := getAuthed(t, reqURL)
		var parsed tempoSearch
		decodeErr := json.NewDecoder(resp.Body).Decode(&parsed)
		resp.Body.Close()
		if decodeErr != nil {
			lastErr = decodeErr
			time.Sleep(1 * time.Second)
			continue
		}
		lastBody = parsed
		if len(parsed.Traces) > 0 {
			return
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("timed out waiting for a trace matching %s in Tempo: lastErr=%v lastBody=%+v", traceQL, lastErr, lastBody)
}

// TestDistributedTracingSpansReachTempo is OBS-001's acceptance test: real spans named
// invoke_agent, chat and execute_tool (OTel GenAI semantic conventions), emitted by the actual
// production instrumentation in runcontroller/modelgateway/toolgw handlers, flow through the real
// OTel Collector into a real Tempo and are found there via TraceQL. Self-skips without
// AEON_TEST_OTEL_ENDPOINT/AEON_TEST_TEMPO_QUERY_URL — see make test-go-integration, which brings up
// otel-collector and tempo alongside postgres/temporal/worker.
func TestDistributedTracingSpansReachTempo(t *testing.T) {
	tempoURL := os.Getenv("AEON_TEST_TEMPO_QUERY_URL")
	if tempoURL == "" {
		t.Skip("AEON_TEST_TEMPO_QUERY_URL not set — skipping tracing integration test (see make test-go-integration)")
	}
	flush := ensureTestTracing(t)

	ctx := context.Background()
	marker := fmt.Sprintf("obs001-%d", time.Now().UnixNano())

	// "chat" span, via the real Model Gateway routing code (go/internal/modelgateway).
	gw := modelgateway.New()
	gw.RegisterProvider("fake", tracingFakeProvider{})
	if _, err := gw.Decide(ctx, []modelgateway.Candidate{{Provider: "fake", Model: marker, Priority: 0}}, map[string]any{}, ""); err != nil {
		t.Fatalf("Gateway.Decide: %v", err)
	}

	// "execute_tool" span, via the real Tool Gateway HTTP handler (policy-checked path). The
	// marker tool name matches no permit and is denied — the span is emitted either way.
	// REGISTERED, so the marker is a catalogue name and not one invented on the wire. OBS-016 stopped
	// setting `gen_ai.tool.name` for a tool the gateway does not know, and this test used to rely on
	// exactly that — its comment below still records that the call is DENIED, which remains true and
	// is the half worth keeping: the span is emitted either way.
	toolSrv := newTestServerKnowing(t, marker)
	postExecute(t, toolSrv, "deep-research-general@0.1.0", marker)

	// "invoke_agent" span, via the real Run Controller HTTP handler and a real Temporal server.
	runSrv := newRunControllerTestServer(t)
	startRun(t, runSrv, marker, simpleGraph("obs001-tracing-test.txt"))

	if err := flush(ctx); err != nil {
		t.Fatalf("flushing spans: %v", err)
	}

	// OBS-015: the span is named `chat {model}`. The NAME and the ATTRIBUTE are both asserted, and
	// keeping both is the point — the name is derived from the attribute, so a test that checked only
	// the name would still pass if `gen_ai.request.model` were dropped.
	waitForTempoSpan(t, tempoURL, fmt.Sprintf(`{ name = "chat %s" && span.gen_ai.request.model = "%s" }`, marker, marker), 30*time.Second)
	// OBS-016: `execute_tool {tool}`, and the name exists only for a tool the catalogue has — which is
	// why the server above now registers the marker. The first rewrite of this assertion tried to
	// anchor the tool span to this run's chat span with `&&` ("same trace") and FAILED, measured: this
	// test drives three surfaces independently and does NOT propagate between them, so its three
	// spans are three traces. That one trace holds all three is the PYTHON test's claim
	// (test_end_to_end_tracing.py), not this one's.
	waitForTempoSpan(t, tempoURL, fmt.Sprintf(`{ name = "execute_tool %s" && span.gen_ai.tool.name = "%s" }`, marker, marker), 30*time.Second)
	waitForTempoSpan(t, tempoURL, fmt.Sprintf(`{ name = "invoke_agent" && span.gen_ai.agent.name = "%s" }`, marker), 30*time.Second)
}
