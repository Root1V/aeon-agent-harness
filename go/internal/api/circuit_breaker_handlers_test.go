package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aeon-ai/aeon/go/internal/circuitbreaker"
	"github.com/aeon-ai/aeon/go/internal/runcontroller"
	"github.com/aeon-ai/aeon/go/internal/secrets"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// randSuffix avoids name/run-id collisions across repeated runs against the same shared Postgres
// and Temporal servers — same reasoning as store.randSuffix, duplicated here since it's unexported
// in a different package.
func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// newCircuitBreakerTestStore mirrors newMemoryTestServer's memoryTestDSN-based connection —
// store.newTestStore isn't reachable from this package (unexported there).
func newCircuitBreakerTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Connect(context.Background(), memoryTestDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// releaseRealAgent registers and walks a fresh, real agent version to Released against real
// Postgres — the only lifecycle state Quarantine (and this test) cares about.
func releaseRealAgent(t *testing.T, registry *store.AgentRegistry, name, version string) {
	t.Helper()
	ctx := context.Background()
	manifest := map[string]any{
		"apiVersion": "harness.ai/v1",
		"kind":       "Agent",
		"metadata":   map[string]any{"name": name, "version": version},
		"spec": map[string]any{
			"modelPolicy":   map[string]any{"profile": "reasoning-high"},
			"tools":         map[string]any{"allow": []any{"search.web"}},
			"runtime":       map[string]any{"maxTurns": 24},
			"contextPolicy": map[string]any{},
		},
	}
	if _, err := registry.Create(ctx, manifest, "test-owner"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := registry.TransitionLifecycle(ctx, name, version, store.LifecycleCandidate, store.ReleaseGateDecision{}); err != nil {
		t.Fatalf("Draft->Candidate: %v", err)
	}
	if _, err := registry.TransitionLifecycle(ctx, name, version, store.LifecycleReleased, store.ReleaseGateDecision{Allowed: true}); err != nil {
		t.Fatalf("Candidate->Released: %v", err)
	}
}

// TestCircuitBreakerQuarantinesVersion is A5's acceptance test: reporting enough failing outcomes
// for a Released agent version trips the breaker, durably quarantines it in the real registry,
// revokes an outstanding secret lease tagged with that agent's identity, and — the actual
// enforcement point — a subsequent POST /runs naming that agent's ref is refused before the Run
// Controller ever touches Temporal (proven against a real Temporal server: the "before trip" run
// genuinely starts, the "after trip" one is rejected with no workflow ever created). Unquarantine
// reverses the block.
func TestCircuitBreakerQuarantinesVersion(t *testing.T) {
	s := newCircuitBreakerTestStore(t)
	registry := s.AgentRegistry()
	name := "circuit-breaker-test-agent-" + randSuffix(t)
	version := "0.1.0"
	releaseRealAgent(t, registry, name, version)
	agentRef := name + "@" + version

	broker := secrets.NewBroker(map[string]string{"demo": "sk-do-not-leak"})
	leaseRef, _, err := broker.IssueForOwner("demo", time.Minute, agentRef)
	if err != nil {
		t.Fatalf("IssueForOwner: %v", err)
	}

	breaker := circuitbreaker.New(circuitbreaker.Thresholds{WindowSize: 4, MinSamples: 4, MaxFailureRate: 0.5})
	temporalClient := testTemporalClient(t)

	mux := http.NewServeMux()
	(&CircuitBreakerHandlers{Registry: registry, Breaker: breaker, Secrets: broker}).Register(mux)
	(&RunControllerHandlers{Controller: runcontroller.New(temporalClient, "test-queue-"+randSuffix(t)), Registry: registry}).Register(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	postOutcome := func(success bool) (int, map[string]any) {
		body, _ := json.Marshal(recordOutcomeRequest{Success: success})
		resp, err := http.Post(srv.URL+"/agents/"+name+"/"+version+"/outcomes", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST outcomes: %v", err)
		}
		defer resp.Body.Close()
		var parsed map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&parsed)
		return resp.StatusCode, parsed
	}

	t.Run("run starts normally before any bad outcomes", func(t *testing.T) {
		status, body := postStartRun(t, srv, "run-before-trip-"+randSuffix(t), agentRef)
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (a real Temporal workflow should start); body = %v", status, body)
		}
	})

	// Three failures, one success: failure rate 0.75 > 0.5 threshold, at exactly MinSamples.
	for i := 0; i < 3; i++ {
		status, body := postOutcome(false)
		if status != http.StatusOK {
			t.Fatalf("POST outcomes (failure %d): status=%d body=%v", i, status, body)
		}
		if tripped, _ := body["tripped"].(bool); tripped {
			t.Fatalf("did not expect a trip before MinSamples, got body=%v", body)
		}
	}
	status, body := postOutcome(true)
	if status != http.StatusOK {
		t.Fatalf("POST outcomes (final): status=%d body=%v", status, body)
	}
	if tripped, _ := body["tripped"].(bool); !tripped {
		t.Fatalf("expected the breaker to trip, got body=%v", body)
	}

	t.Run("the registry durably reflects the quarantine", func(t *testing.T) {
		rec, err := registry.Get(context.Background(), name, version)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !rec.Quarantined {
			t.Fatal("expected Quarantined = true")
		}
		if rec.QuarantineReason == "" {
			t.Fatal("expected a non-empty QuarantineReason")
		}
		if rec.Lifecycle != store.LifecycleReleased {
			t.Fatalf("lifecycle = %q, want unchanged %q", rec.Lifecycle, store.LifecycleReleased)
		}
	})

	t.Run("the agent's outstanding secret lease was revoked", func(t *testing.T) {
		if _, err := broker.Resolve(leaseRef); err == nil {
			t.Fatal("expected the lease to have been revoked on quarantine")
		}
	})

	t.Run("a new run for the quarantined version is refused before reaching the Controller", func(t *testing.T) {
		status, body := postStartRun(t, srv, "run-after-trip-"+randSuffix(t), agentRef)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", status, body)
		}
		if errMsg, _ := body["error"].(string); errMsg == "" {
			t.Fatal("expected a non-empty error message explaining the quarantine")
		}
	})

	t.Run("a run for an unrelated, non-quarantined agent ref is unaffected", func(t *testing.T) {
		status, body := postStartRun(t, srv, "run-other-agent-"+randSuffix(t), "some-other-agent@1.0.0")
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201 for an agent ref that was never quarantined; body=%v", status, body)
		}
	})

	t.Run("unquarantine reverses the block", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/agents/"+name+"/"+version+"/unquarantine", "application/json", bytes.NewReader([]byte("{}")))
		if err != nil {
			t.Fatalf("POST unquarantine: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST unquarantine: status = %d", resp.StatusCode)
		}

		status, body := postStartRun(t, srv, "run-after-unquarantine-"+randSuffix(t), agentRef)
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201 after unquarantine; body=%v", status, body)
		}
	})
}

func postStartRun(t *testing.T, srv *httptest.Server, runID, agentManifestRef string) (int, map[string]any) {
	t.Helper()
	reqBody, _ := json.Marshal(startRunRequest{
		RunID:            runID,
		Graph:            map[string]any{"nodes": []any{}},
		AgentManifestRef: agentManifestRef,
	})
	resp, err := http.Post(srv.URL+"/runs", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

// TestQuarantineHandlerIsAKillSwitchRegardlessOfBreakerState is A5's manual-trip path: an operator
// can quarantine a Released version immediately, with no rolling-window threshold involved at all.
func TestQuarantineHandlerIsAKillSwitchRegardlessOfBreakerState(t *testing.T) {
	s := newCircuitBreakerTestStore(t)
	registry := s.AgentRegistry()
	name := "kill-switch-test-agent-" + randSuffix(t)
	version := "0.1.0"
	releaseRealAgent(t, registry, name, version)

	breaker := circuitbreaker.New(circuitbreaker.DefaultThresholds)
	mux := http.NewServeMux()
	(&CircuitBreakerHandlers{Registry: registry, Breaker: breaker}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(quarantineRequest{Reason: "operator-triggered kill switch, suspected data exfiltration"})
	resp, err := http.Post(srv.URL+"/agents/"+name+"/"+version+"/quarantine", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST quarantine: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	rec, err := registry.Get(context.Background(), name, version)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !rec.Quarantined {
		t.Fatal("expected Quarantined = true immediately, no threshold needed")
	}
}

// TestQuarantineRejectsNonReleasedVersion confirms the store-level ErrNotReleased guard surfaces as
// a real, distinguishable HTTP status (409) rather than a generic 500.
func TestQuarantineRejectsNonReleasedVersion(t *testing.T) {
	s := newCircuitBreakerTestStore(t)
	registry := s.AgentRegistry()
	name := "draft-agent-" + randSuffix(t)
	version := "0.1.0"
	ctx := context.Background()
	manifest := map[string]any{
		"apiVersion": "harness.ai/v1",
		"kind":       "Agent",
		"metadata":   map[string]any{"name": name, "version": version},
		"spec":       map[string]any{"modelPolicy": map[string]any{"profile": "reasoning-high"}, "tools": map[string]any{}, "runtime": map[string]any{}, "contextPolicy": map[string]any{}},
	}
	if _, err := registry.Create(ctx, manifest, "test-owner"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	breaker := circuitbreaker.New(circuitbreaker.DefaultThresholds)
	mux := http.NewServeMux()
	(&CircuitBreakerHandlers{Registry: registry, Breaker: breaker}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(quarantineRequest{Reason: "should not apply to a Draft"})
	resp, err := http.Post(srv.URL+"/agents/"+name+"/"+version+"/quarantine", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST quarantine: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a non-Released version", resp.StatusCode)
	}
}
