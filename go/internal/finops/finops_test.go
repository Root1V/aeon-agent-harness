package finops

import "testing"

func TestCostUSDComputesRealCostFromRealUsage(t *testing.T) {
	table := NewPricingTable([]Rate{
		{Provider: "anthropic", Model: "claude-opus-5", CostModel: "token_based", InputPerMillionUSD: 15, OutputPerMillionUSD: 75},
	})

	usd, ok := table.CostUSD("anthropic", "claude-opus-5", 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("expected CostUSD to succeed for a configured token_based rate")
	}
	want := 15.0 + 75.0
	if usd != want {
		t.Fatalf("CostUSD = %v, want %v", usd, want)
	}
}

func TestCostUSDScalesWithPartialMillionTokens(t *testing.T) {
	table := NewPricingTable([]Rate{
		{Provider: "openai", Model: "gpt-5.1", CostModel: "token_based", InputPerMillionUSD: 10, OutputPerMillionUSD: 30},
	})

	usd, ok := table.CostUSD("openai", "gpt-5.1", 500_000, 100_000)
	if !ok {
		t.Fatal("expected CostUSD to succeed")
	}
	want := 500_000.0/1_000_000*10 + 100_000.0/1_000_000*30
	if usd != want {
		t.Fatalf("CostUSD = %v, want %v", usd, want)
	}
}

func TestCostUSDUnknownForUnconfiguredModel(t *testing.T) {
	table := NewPricingTable(nil)
	if _, ok := table.CostUSD("anthropic", "claude-opus-5", 100, 100); ok {
		t.Fatal("expected ok=false for a model with no configured rate")
	}
}

func TestCostUSDUnknownForComputeBasedProvider(t *testing.T) {
	table := NewPricingTable([]Rate{
		{Provider: "prometheus_inference", Model: "local-default", CostModel: "compute_based"},
	})
	if _, ok := table.CostUSD("prometheus_inference", "local-default", 1000, 1000); ok {
		t.Fatal("expected ok=false for a compute_based provider — per-token cost isn't meaningful for it")
	}
}

func TestCostUSDOnNilTableIsUnknownNotPanic(t *testing.T) {
	var table *PricingTable
	if _, ok := table.CostUSD("anthropic", "claude-opus-5", 100, 100); ok {
		t.Fatal("expected ok=false on a nil table")
	}
}

func TestNewPricingTableLaterEntryReplacesEarlier(t *testing.T) {
	table := NewPricingTable([]Rate{
		{Provider: "openai", Model: "gpt-5.1", CostModel: "token_based", InputPerMillionUSD: 1, OutputPerMillionUSD: 1},
		{Provider: "openai", Model: "gpt-5.1", CostModel: "token_based", InputPerMillionUSD: 10, OutputPerMillionUSD: 30},
	})
	r, ok := table.Rate("openai", "gpt-5.1")
	if !ok || r.InputPerMillionUSD != 10 {
		t.Fatalf("expected the later rate to win, got %+v", r)
	}
}
