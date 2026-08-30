package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func testAgentManifest(name, version string) map[string]any {
	return map[string]any{
		"apiVersion": "harness.ai/v1",
		"kind":       "Agent",
		"metadata": map[string]any{
			"name":    name,
			"version": version,
		},
		"spec": map[string]any{
			"modelPolicy":   map[string]any{"profile": "reasoning-high"},
			"tools":         map[string]any{"allow": []any{"search.web"}},
			"runtime":       map[string]any{"maxTurns": 24},
			"contextPolicy": map[string]any{},
		},
	}
}

// TestAgentRegistryLifecycle is FND-001's acceptance test (see roadmap.md): create a manifest,
// confirm it lands as Draft, walk it forward one step at a time to Retired, and confirm every
// out-of-order or repeated transition is rejected — the lifecycle is forward-only, one step at a
// time, exactly as docs/adr and the spec require.
func TestAgentRegistryLifecycle(t *testing.T) {
	s := newTestStore(t)
	registry := s.AgentRegistry()
	ctx := context.Background()

	name := "test-agent-" + randSuffix(t)
	version := "0.1.0"

	created, err := registry.Create(ctx, testAgentManifest(name, version), "test-owner")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Lifecycle != LifecycleDraft {
		t.Fatalf("new agent lifecycle = %q, want %q", created.Lifecycle, LifecycleDraft)
	}

	// Creating the same (name, version) again must fail — versions are immutable.
	if _, err := registry.Create(ctx, testAgentManifest(name, version), "test-owner"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate Create: got %v, want ErrAlreadyExists", err)
	}

	// Skipping a state must be rejected (Draft -> Released is not a single step).
	if _, err := registry.TransitionLifecycle(ctx, name, version, LifecycleReleased, ReleaseGateDecision{}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Draft->Released: got %v, want ErrInvalidTransition", err)
	}

	// Walk the lifecycle forward one step at a time. The Candidate -> Released step needs an
	// Allowed release gate decision (EVAL-003) — every other step ignores it.
	forward := []string{LifecycleCandidate, LifecycleReleased, LifecycleRetired}
	for _, target := range forward {
		gate := ReleaseGateDecision{}
		if target == LifecycleReleased {
			gate = ReleaseGateDecision{Allowed: true, Reason: "eval suite passed with no regression (test fixture)"}
		}
		rec, err := registry.TransitionLifecycle(ctx, name, version, target, gate)
		if err != nil {
			t.Fatalf("transition to %s: %v", target, err)
		}
		if rec.Lifecycle != target {
			t.Fatalf("after transition, lifecycle = %q, want %q", rec.Lifecycle, target)
		}
	}

	// Retired is terminal: no further transition is valid, including re-requesting Retired.
	if _, err := registry.TransitionLifecycle(ctx, name, version, LifecycleRetired, ReleaseGateDecision{}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("transition from terminal Retired: got %v, want ErrInvalidTransition", err)
	}

	// A manifest missing metadata.name/version must be rejected before ever reaching Postgres.
	if _, err := registry.Create(ctx, map[string]any{"metadata": map[string]any{}}, "test-owner"); err == nil {
		t.Fatalf("Create with missing name/version: expected error, got nil")
	}

	got, err := registry.Get(ctx, name, version)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Owner != "test-owner" {
		t.Fatalf("Get: owner = %q, want %q", got.Owner, "test-owner")
	}

	all, err := registry.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, rec := range all {
		if rec.Name == name && rec.Version == version {
			found = true
		}
	}
	if !found {
		t.Fatalf("List did not include the agent just created")
	}

	if _, err := registry.Get(ctx, "does-not-exist", "0.0.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing agent: got %v, want ErrNotFound", err)
	}
}

// TestAgentRegistryReleaseGateBlocksPromotion is EVAL-003's registry-side integration test: a
// Candidate can't reach Released without an Allowed ReleaseGateDecision (computed elsewhere, by
// aeon_evalops.release_gate.evaluate_release_gate — see python/tests/unit/test_release_gate.py's
// test_release_gate_blocks_regression for the actual regression-detection logic this store applies
// but never computes itself), and a blocked promotion leaves the agent's lifecycle unchanged.
func TestAgentRegistryReleaseGateBlocksPromotion(t *testing.T) {
	s := newTestStore(t)
	registry := s.AgentRegistry()
	ctx := context.Background()

	name := "test-agent-gate-" + randSuffix(t)
	version := "0.1.0"
	if _, err := registry.Create(ctx, testAgentManifest(name, version), "test-owner"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := registry.TransitionLifecycle(ctx, name, version, LifecycleCandidate, ReleaseGateDecision{}); err != nil {
		t.Fatalf("Draft->Candidate: %v", err)
	}

	t.Run("a blocked gate rejects the promotion and leaves the agent at Candidate", func(t *testing.T) {
		_, err := registry.TransitionLifecycle(ctx, name, version, LifecycleReleased, ReleaseGateDecision{
			Allowed: false, Reason: "citation_integrity_grader regressed from 1.000 to 0.900",
		})
		if !errors.Is(err, ErrReleaseGateBlocked) {
			t.Fatalf("got %v, want ErrReleaseGateBlocked", err)
		}
		if !strings.Contains(err.Error(), "regressed") {
			t.Errorf("expected the gate's own reason in the error, got: %v", err)
		}

		rec, getErr := registry.Get(ctx, name, version)
		if getErr != nil {
			t.Fatalf("Get: %v", getErr)
		}
		if rec.Lifecycle != LifecycleCandidate {
			t.Fatalf("lifecycle after a blocked promotion = %q, want unchanged %q", rec.Lifecycle, LifecycleCandidate)
		}
	})

	t.Run("an allowed gate lets the same promotion through", func(t *testing.T) {
		rec, err := registry.TransitionLifecycle(ctx, name, version, LifecycleReleased, ReleaseGateDecision{Allowed: true})
		if err != nil {
			t.Fatalf("TransitionLifecycle with an allowed gate: %v", err)
		}
		if rec.Lifecycle != LifecycleReleased {
			t.Fatalf("lifecycle = %q, want %q", rec.Lifecycle, LifecycleReleased)
		}
	})

	t.Run("the gate is ignored for every transition other than Candidate->Released", func(t *testing.T) {
		if _, err := registry.TransitionLifecycle(ctx, name, version, LifecycleRetired, ReleaseGateDecision{Allowed: false}); err != nil {
			t.Fatalf("Released->Retired should ignore the gate entirely, got: %v", err)
		}
	})
}
