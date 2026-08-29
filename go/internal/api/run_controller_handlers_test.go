package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/aeon-ai/aeon/go/internal/runcontroller"
)

// testTemporalClient connects to a real Temporal server for RUN-001's acceptance test
// (test_run_controller_lifecycle in roadmap.md). Self-skips like the Postgres-backed registry
// tests (go/internal/store/testing.go) when the address isn't provided — see make
// test-go-integration, which brings up both `temporal` and `worker` from the compose stack.
func testTemporalClient(t *testing.T) client.Client {
	t.Helper()
	addr := os.Getenv("AEON_TEST_TEMPORAL_ADDRESS")
	if addr == "" {
		t.Skip("AEON_TEST_TEMPORAL_ADDRESS not set — skipping Temporal integration test (see make test-go-integration)")
	}
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("connecting to Temporal at %s: %v", addr, err)
	}
	t.Cleanup(c.Close)
	return c
}

func newRunControllerTestServer(t *testing.T) *httptest.Server {
	c := testTemporalClient(t)
	mux := http.NewServeMux()
	handlers := &RunControllerHandlers{Controller: runcontroller.New(c, "")}
	handlers.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// simpleGraph is deliberately trivial (a single tool_call) — RUN-002's own node-kind coverage
// lives in test_graph_runtime_node_kinds; this test only needs *a* graph a real worker can run.
func simpleGraph(path string) map[string]any {
	return map[string]any{
		"id": "n0", "kind": "tool_call", "tool_name": "artifact.write",
		"tool_args": map[string]any{"path": path},
	}
}

func newRunID(label string) string {
	return fmt.Sprintf("rc-%s-%d", label, time.Now().UnixNano())
}

func startRun(t *testing.T, srv *httptest.Server, runID string, graph map[string]any) {
	t.Helper()
	startRunWithBudgets(t, srv, runID, graph, nil)
}

func startRunWithBudgets(t *testing.T, srv *httptest.Server, runID string, graph, budgets map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(startRunRequest{RunID: runID, Graph: graph, Budgets: budgets})
	resp, err := http.Post(srv.URL+"/runs", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /runs status = %d, want 201", resp.StatusCode)
	}
}

func postAction(t *testing.T, srv *httptest.Server, runID, action string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/runs/"+runID+"/"+action, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /runs/%s/%s: %v", runID, action, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /runs/%s/%s status = %d, want 202", runID, action, resp.StatusCode)
	}
}

func getStatus(t *testing.T, srv *httptest.Server, runID string) map[string]any {
	t.Helper()
	resp, err := http.Get(srv.URL + "/runs/" + runID)
	if err != nil {
		t.Fatalf("GET /runs/%s: %v", runID, err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	return parsed
}

func waitForStatus(t *testing.T, srv *httptest.Server, runID, want string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		last = getStatus(t, srv, runID)
		if last["status"] == want {
			return last
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for run %s to reach status=%s, last observed: %v", runID, want, last)
	return nil
}

// TestRunControllerLifecycle is RUN-001's acceptance test: start/cancel/pause/resume/status/stream
// against a real Temporal server and a real worker (the same aeon-worker container the compose
// stack runs), driven entirely through the Run Controller's HTTP API.
func TestRunControllerLifecycle(t *testing.T) {
	srv := newRunControllerTestServer(t)

	t.Run("pause blocks progress, resume lets it complete", func(t *testing.T) {
		runID := newRunID("pause")
		startRun(t, srv, runID, simpleGraph("pause-test.txt"))

		// Paused before the workflow's first task runs (graph.py checks the pause gate before
		// every node, including the very first) — so this is not a race: however fast the worker
		// is, it cannot get past node n0 until resumed.
		postAction(t, srv, runID, "pause")

		time.Sleep(1 * time.Second)
		status := getStatus(t, srv, runID)
		// mapStatus (controller.go) reports "PAUSED", not "RUNNING", once the is_paused query
		// returns true — distinct from the raw Temporal execution status, which stays RUNNING the
		// whole time (Temporal has no native "paused" state; ours is purely workflow-side).
		if status["status"] != "PAUSED" {
			t.Fatalf("expected status=PAUSED while paused, got %v", status)
		}
		if paused, _ := status["paused"].(bool); !paused {
			t.Fatalf("expected paused=true, got %v", status)
		}

		postAction(t, srv, runID, "resume")
		final := waitForStatus(t, srv, runID, "SUCCEEDED", 15*time.Second)
		if paused, _ := final["paused"].(bool); paused {
			t.Fatalf("expected paused=false once succeeded, got %v", final)
		}
	})

	t.Run("cancel while paused reliably reaches CANCELLED", func(t *testing.T) {
		runID := newRunID("cancel")
		startRun(t, srv, runID, simpleGraph("cancel-test.txt"))
		// Same pause trick, but this time to make the cancel target deterministic: without it, a
		// single-node graph might already be SUCCEEDED before the cancel request lands.
		postAction(t, srv, runID, "pause")
		time.Sleep(500 * time.Millisecond)

		postAction(t, srv, runID, "cancel")
		waitForStatus(t, srv, runID, "CANCELLED", 15*time.Second)
	})

	t.Run("stream reports the terminal status", func(t *testing.T) {
		runID := newRunID("stream")
		startRun(t, srv, runID, simpleGraph("stream-test.txt"))

		resp, err := http.Get(srv.URL + "/runs/" + runID + "/stream")
		if err != nil {
			t.Fatalf("GET /runs/%s/stream: %v", runID, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream status = %d, want 200", resp.StatusCode)
		}

		scanner := bufio.NewScanner(resp.Body)
		var sawSucceeded bool
		deadline := time.Now().Add(15 * time.Second)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"SUCCEEDED"`) {
				sawSucceeded = true
				break
			}
			if time.Now().After(deadline) {
				break
			}
		}
		if !sawSucceeded {
			t.Fatalf("stream for run %s never reported SUCCEEDED", runID)
		}
	})

	t.Run("budgets are enforced end to end through the HTTP API", func(t *testing.T) {
		// RUN-003's hard stop, exercised through this Go layer rather than just graph.py directly
		// (see test_budget_hard_stop.py for full dimension coverage): a loop willing to run 5
		// iterations, but the budget only allows 2 — the run must fail, and the failure must be
		// observable through the same Status endpoint every other scenario uses.
		runID := newRunID("budget")
		graph := map[string]any{
			"id": "root", "kind": "loop", "max_iterations": 5,
			"body": simpleGraph("budget-test.txt"),
		}
		startRunWithBudgets(t, srv, runID, graph, map[string]any{"max_tool_calls": 2})

		final := waitForStatus(t, srv, runID, "FAILED", 15*time.Second)
		consumed, ok := final["budgets_consumed"].(map[string]any)
		if !ok {
			t.Fatalf("expected budgets_consumed in status, got %v", final)
		}
		if toolCalls, _ := consumed["tool_calls"].(float64); toolCalls != 2 {
			t.Fatalf("expected exactly 2 tool calls to have run before the hard stop, got %v", consumed)
		}
	})
}
