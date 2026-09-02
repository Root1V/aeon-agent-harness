package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aeon-ai/aeon/go/internal/runcontroller"
)

// TestAgentConsoleShowsARunEndToEnd is OBS-002's acceptance test: a real run, started through the
// real Run Controller HTTP API against a real Temporal server and a real worker, completes for
// real; its real invoke_agent span reaches a real Tempo; and the Agent Console's HTML page for that
// run_id shows both the real terminal status and the real trace Tempo actually recorded — a UI
// showing one real run end to end, not a fixture.
func TestAgentConsoleShowsARunEndToEnd(t *testing.T) {
	tempoURL := os.Getenv("AEON_TEST_TEMPO_QUERY_URL")
	if tempoURL == "" {
		t.Skip("AEON_TEST_TEMPO_QUERY_URL not set — skipping tracing integration test (see make test-go-integration)")
	}
	temporalClient := testTemporalClient(t)
	flush := ensureTestTracing(t)
	ctx := context.Background()

	controller := runcontroller.New(temporalClient, "")
	mux := http.NewServeMux()
	(&RunControllerHandlers{Controller: controller}).Register(mux)
	(&ConsoleHandlers{Controller: controller, TempoURL: tempoURL}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	runID := newRunID("console")
	startRun(t, srv, runID, simpleGraph("obs002-console-test.txt"))
	waitForStatus(t, srv, runID, "SUCCEEDED", 30*time.Second)

	if err := flush(ctx); err != nil {
		t.Fatalf("flushing spans: %v", err)
	}
	waitForTempoSpan(t, tempoURL, `{ name = "invoke_agent" && span.gen_ai.agent.name = "`+runID+`" }`, 30*time.Second)

	resp, err := http.Get(srv.URL + "/console/runs/" + runID)
	if err != nil {
		t.Fatalf("GET /console/runs/%s: %v", runID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	page := string(body)

	if !strings.Contains(page, runID) {
		t.Errorf("expected the page to show the run_id %q, got:\n%s", runID, page)
	}
	if !strings.Contains(page, "SUCCEEDED") {
		t.Errorf("expected the page to show the real terminal status SUCCEEDED, got:\n%s", page)
	}
	if !strings.Contains(page, "invoke_agent") {
		t.Errorf("expected the page to show the real trace's root span name (invoke_agent), got:\n%s", page)
	}
	if strings.Contains(page, "no traces found") {
		t.Errorf("expected real trace rows, not the empty-state message, got:\n%s", page)
	}
}

// TestAgentConsoleReturns404ForAnUnknownRun confirms an unknown run_id is reported honestly rather
// than as a generic 500 or a page pretending the run exists.
func TestAgentConsoleReturns404ForAnUnknownRun(t *testing.T) {
	temporalClient := testTemporalClient(t)
	controller := runcontroller.New(temporalClient, "")
	mux := http.NewServeMux()
	(&ConsoleHandlers{Controller: controller}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/console/runs/" + newRunID("does-not-exist"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
