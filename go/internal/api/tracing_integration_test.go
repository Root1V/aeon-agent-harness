package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/tracing"
)

// tracingFakeProvider is a minimal providers.Provider test double, mirroring
// modelgateway.fakeProvider — duplicated here rather than imported since that one is unexported
// test-only code in a different package.
type tracingFakeProvider struct{}

func (tracingFakeProvider) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	return map[string]any{"model": renderedContext["model"]}, nil
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
		resp, err := http.Get(reqURL)
		if err != nil {
			lastErr = err
			time.Sleep(1 * time.Second)
			continue
		}
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
	otelEndpoint := os.Getenv("AEON_TEST_OTEL_ENDPOINT")
	tempoURL := os.Getenv("AEON_TEST_TEMPO_QUERY_URL")
	if otelEndpoint == "" || tempoURL == "" {
		t.Skip("AEON_TEST_OTEL_ENDPOINT/AEON_TEST_TEMPO_QUERY_URL not set — skipping tracing integration test (see make test-go-integration)")
	}

	ctx := context.Background()
	_, shutdown, err := tracing.Init(ctx, "aeon-obs001-integration-test", otelEndpoint)
	if err != nil {
		t.Fatalf("tracing.Init: %v", err)
	}

	marker := fmt.Sprintf("obs001-%d", time.Now().UnixNano())

	// "chat" span, via the real Model Gateway routing code (go/internal/modelgateway).
	gw := modelgateway.New()
	gw.RegisterProvider("fake", tracingFakeProvider{})
	if _, err := gw.Decide(ctx, []modelgateway.Candidate{{Provider: "fake", Model: marker, Priority: 0}}, map[string]any{}, ""); err != nil {
		t.Fatalf("Gateway.Decide: %v", err)
	}

	// "execute_tool" span, via the real Tool Gateway HTTP handler (policy-checked path). The
	// marker tool name matches no permit and is denied — the span is emitted either way.
	toolSrv := newTestServer(t)
	postExecute(t, toolSrv, "deep-research-general@0.1.0", marker)

	// "invoke_agent" span, via the real Run Controller HTTP handler and a real Temporal server.
	runSrv := newRunControllerTestServer(t)
	startRun(t, runSrv, marker, simpleGraph("obs001-tracing-test.txt"))

	if err := shutdown(ctx); err != nil {
		t.Fatalf("tracing shutdown (flush): %v", err)
	}

	waitForTempoSpan(t, tempoURL, fmt.Sprintf(`{ name = "chat" && span.gen_ai.request.model = "%s" }`, marker), 30*time.Second)
	waitForTempoSpan(t, tempoURL, fmt.Sprintf(`{ name = "execute_tool" && span.gen_ai.tool.name = "%s" }`, marker), 30*time.Second)
	waitForTempoSpan(t, tempoURL, fmt.Sprintf(`{ name = "invoke_agent" && span.gen_ai.agent.name = "%s" }`, marker), 30*time.Second)
}
