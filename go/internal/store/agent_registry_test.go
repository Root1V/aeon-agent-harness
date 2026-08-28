package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	if _, err := registry.TransitionLifecycle(ctx, name, version, LifecycleReleased); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Draft->Released: got %v, want ErrInvalidTransition", err)
	}

	// Walk the lifecycle forward one step at a time.
	forward := []string{LifecycleCandidate, LifecycleReleased, LifecycleRetired}
	for _, target := range forward {
		rec, err := registry.TransitionLifecycle(ctx, name, version, target)
		if err != nil {
			t.Fatalf("transition to %s: %v", target, err)
		}
		if rec.Lifecycle != target {
			t.Fatalf("after transition, lifecycle = %q, want %q", rec.Lifecycle, target)
		}
	}

	// Retired is terminal: no further transition is valid, including re-requesting Retired.
	if _, err := registry.TransitionLifecycle(ctx, name, version, LifecycleRetired); !errors.Is(err, ErrInvalidTransition) {
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
