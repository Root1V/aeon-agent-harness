package store

import (
	"context"
	"testing"
)

// TestFinOpsLedgerRecordsAndAggregatesRealCosts is OBS-003's storage-layer acceptance test: real
// cost events, written to real Postgres, come back as a correct real SQL aggregation grouped by
// (provider, model) — call count, total cost, and total token usage all genuinely summed, not
// computed in Go from a re-read of individual rows.
func TestFinOpsLedgerRecordsAndAggregatesRealCosts(t *testing.T) {
	s := newTestStore(t)
	ledger := s.FinOpsLedger()
	ctx := context.Background()

	// A unique model name per test run avoids collisions with rows any other test run left behind
	// in the same shared Postgres (see store.randSuffix's rationale elsewhere in this package).
	model := "test-model-" + randSuffix(t)

	entries := []CostEntry{
		{Provider: "anthropic", Model: model, CostModel: tokenBased(), PromptTokens: 1000, CompletionTokens: 500, CostUSD: usd(0.05), RunID: "run-1"},
		{Provider: "anthropic", Model: model, CostModel: tokenBased(), PromptTokens: 2000, CompletionTokens: 1000, CostUSD: usd(0.10)},
	}
	for _, e := range entries {
		if err := ledger.Record(ctx, e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	totals, err := ledger.TotalsByModel(ctx)
	if err != nil {
		t.Fatalf("TotalsByModel: %v", err)
	}

	var found *ModelTotal
	for i := range totals {
		if totals[i].Provider == "anthropic" && totals[i].Model == model {
			found = &totals[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected a total for (anthropic, %s), got %+v", model, totals)
	}
	if found.CallCount != 2 {
		t.Fatalf("CallCount = %d, want 2", found.CallCount)
	}
	// Floating-point sums (0.05 + 0.10 in IEEE754 double precision, summed by Postgres itself) are
	// not exactly 0.15 — a real property of DOUBLE PRECISION arithmetic, not a bug here or in the
	// SQL. An epsilon comparison is the correct check, not exact equality.
	if found.TotalCostUSD == nil {
		t.Fatal("TotalCostUSD is nil for a group where every call was priced")
	}
	if diff := *found.TotalCostUSD - 0.15; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("TotalCostUSD = %v, want ~0.15", *found.TotalCostUSD)
	}
	if found.UnpricedCalls != 0 {
		t.Fatalf("UnpricedCalls = %d, want 0 — every call in this group carried a cost", found.UnpricedCalls)
	}
	if found.TotalPromptTokens != 3000 || found.TotalCompletionTokens != 1500 {
		t.Fatalf("token totals = (%d, %d), want (3000, 1500)", found.TotalPromptTokens, found.TotalCompletionTokens)
	}
	if found.CostModel == nil || *found.CostModel != "token_based" {
		t.Fatalf("CostModel = %v, want token_based", found.CostModel)
	}
}

func TestFinOpsLedgerRecordWithoutRunIDStoresNull(t *testing.T) {
	s := newTestStore(t)
	ledger := s.FinOpsLedger()
	ctx := context.Background()
	model := "test-model-no-run-" + randSuffix(t)

	if err := ledger.Record(ctx, CostEntry{Provider: "openai", Model: model, CostModel: tokenBased(), PromptTokens: 10, CompletionTokens: 5, CostUSD: usd(0.001)}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	totals, err := ledger.TotalsByModel(ctx)
	if err != nil {
		t.Fatalf("TotalsByModel: %v", err)
	}
	found := false
	for _, tot := range totals {
		if tot.Provider == "openai" && tot.Model == model {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a total for (openai, %s) even with no run_id, got %+v", model, totals)
	}
}

// usd and tokenBased keep the pointer-taking out of the table literals above. The fields are
// pointers because nil and zero are different facts (OBS-008), not because the tests need it.
func usd(v float64) *float64 { return &v }

func tokenBased() *string { s := "token_based"; return &s }
