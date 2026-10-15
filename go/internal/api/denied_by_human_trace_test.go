package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aeon-ai/aeon/go/internal/runcontroller"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// TestAHumanRejectionSaysAPersonDeniedIt is OBS-020's second half: a real run, suspended on a real
// approval, rejected by a real HTTP call, against real Temporal and real Tempo.
//
// `human` is the only one of `argus.denied_by`'s three values that is not the system refusing on its
// own, and it was the one with no producer at all. The rejection was recorded in the RUN'S JOURNAL
// (INT-010) and nowhere in the trace — so "who stopped this run" was answerable from the database
// and not from the place an incident is investigated.
func TestAHumanRejectionSaysAPersonDeniedIt(t *testing.T) {
	tempoURL := os.Getenv("AEON_TEST_TEMPO_QUERY_URL")
	if tempoURL == "" {
		t.Skip("AEON_TEST_TEMPO_QUERY_URL not set — skipping tracing integration test (see make test-go-integration)")
	}
	flush := ensureTestTracing(t)

	c := testTemporalClient(t)
	s, err := store.Connect(context.Background(), memoryTestDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	mux := http.NewServeMux()
	(&RunControllerHandlers{Controller: runcontroller.New(c, ""), Checkpointer: s}).Register(mux)
	(&CheckpointHandlers{Store: s}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux, deniedOutcomeAgent))
	t.Cleanup(srv.Close)

	runID := newRunID("obs020-human")
	startRun(t, srv, runID, approvalGraph("obs020-human-test.txt"))

	paused := waitForStatus(t, srv, runID, "PAUSED_FOR_APPROVAL", 15*time.Second)
	pending, ok := paused["pending_approval"].(map[string]any)
	if !ok {
		t.Fatalf("expected pending_approval in status, got %v", paused)
	}
	toolCallHash, _ := pending["tool_call_hash"].(string)
	if toolCallHash == "" {
		t.Fatalf("the pending approval has no tool_call_hash: %v", pending)
	}

	resp := postApprovalDecision(t, srv, runID, "reject", toolCallHash)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /runs/%s/reject = %d, want 202", runID, resp.StatusCode)
	}

	if err := flush(context.Background()); err != nil {
		t.Fatalf("flushing spans: %v", err)
	}

	// THE ROUTE TEMPLATE IS THE SPAN NAME AND THE RUN ID IS AN ATTRIBUTE, which is the split Argus
	// asked for: naming the span after the live path would mint one span name per run.
	waitForTempoSpan(t, tempoURL, fmt.Sprintf(
		`{ name = "POST /runs/{run_id}/reject" && span.argus.outcome = "denied" && span.argus.denied_by = "human" && span.argus.run.id = "%s" }`,
		runID), 30*time.Second)

	// CONTROL: the run id must NOT be in the span name. Asserted because the failure mode is silent —
	// a name carrying an id works perfectly and costs Argus a metric series per run.
	if n, err := tempoSearchCount(t, tempoURL, fmt.Sprintf(`{ name = "POST /runs/%s/reject" }`, runID)); err != nil || n != 0 {
		t.Fatalf("the span is named after the live path (matched=%d err=%v): one span name per run is the "+
			"unbounded cardinality ARG-AEON-002 made a condition", n, err)
	}
}
