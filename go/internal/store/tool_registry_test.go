package store

import (
	"context"
	"errors"
	"testing"
)

func testToolDescriptor(toolID, version, sideEffect, risk string, idempotencyFields []any) map[string]any {
	d := map[string]any{
		"tool_id":      toolID,
		"version":      version,
		"name":         toolID,
		"side_effect":  sideEffect,
		"risk":         risk,
		"input_schema": map[string]any{"type": "object"},
	}
	if idempotencyFields != nil {
		d["idempotency_key_fields"] = idempotencyFields
	}
	return d
}

// TestToolRegistryCRUDAndRiskClassification is TOOL-001's registry-scoped acceptance test (see
// roadmap.md — this covers the Tool Registry itself; the full test_tool_policy_denies_out_of_
// manifest criterion for TOOL-001/SEC-001 additionally needs the Cedar Policy Engine, still TODO).
func TestToolRegistryCRUDAndRiskClassification(t *testing.T) {
	s := newTestStore(t)
	registry := s.ToolRegistry()
	ctx := context.Background()

	toolID := "search.web." + randSuffix(t)

	created, err := registry.Create(ctx, testToolDescriptor(toolID, "1.0.0", SideEffectReadOnly, RiskLow, nil))
	if err != nil {
		t.Fatalf("Create read-only tool: %v", err)
	}
	if created.Risk != RiskLow || created.SideEffect != SideEffectReadOnly {
		t.Fatalf("created tool risk/side_effect = %s/%s, want %s/%s", created.Risk, created.SideEffect, RiskLow, SideEffectReadOnly)
	}

	// A tool with a side effect beyond READ_ONLY must declare idempotency_key_fields — this is
	// what makes RUN-004's crash-resume guarantee possible for that tool at all.
	writeToolID := "artifact.write." + randSuffix(t)
	if _, err := registry.Create(ctx, testToolDescriptor(writeToolID, "1.0.0", SideEffectWriteIrreversible, RiskHigh, nil)); err == nil {
		t.Fatalf("expected Create to reject a WRITE_IRREVERSIBLE tool with no idempotency_key_fields")
	}

	withKey, err := registry.Create(ctx, testToolDescriptor(writeToolID, "1.0.0", SideEffectWriteIrreversible, RiskHigh, []any{"path"}))
	if err != nil {
		t.Fatalf("Create write tool with idempotency_key_fields: %v", err)
	}
	if withKey.Risk != RiskHigh {
		t.Fatalf("risk = %s, want %s", withKey.Risk, RiskHigh)
	}

	// Invalid enum values must be rejected before ever reaching Postgres's CHECK constraints.
	if _, err := registry.Create(ctx, testToolDescriptor("bad-tool", "1.0.0", "NOT_A_REAL_SIDE_EFFECT", RiskLow, nil)); err == nil {
		t.Fatalf("expected Create to reject an invalid side_effect value")
	}

	all, err := registry.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, rec := range all {
		if rec.ToolID == toolID {
			found = true
		}
	}
	if !found {
		t.Fatalf("List did not include the tool just created")
	}

	if _, err := registry.Get(ctx, "does-not-exist", "0.0.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing tool: got %v, want ErrNotFound", err)
	}
}
