package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/providers/anthropic"
)

// fakeAnthropicUpstream speaks the real Messages API response shape. The point of going through the
// real adapter rather than a fake provider is that MDL-012's bug lived in the adapter's mapping,
// not in the ledger — a fake that already returns normalized usage would test everything except the
// thing that was broken.
func fakeAnthropicUpstream(t *testing.T, usage map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"id":          "msg_01",
			"model":       "claude-cache-test",
			"content":     []any{map[string]any{"type": "text", "text": "hola"}},
			"stop_reason": "end_turn",
			"usage":       usage,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAnthropicCacheTokensReachTheLedger is MDL-012's acceptance test.
//
// Before this, anthropic.go mapped usage.input_tokens straight to prompt_tokens and did not parse
// the cache counters at all, so every token served from Anthropic's prompt cache was invisible to
// OBS-003. The direction of that error is what made it worth a feature of its own: ADR-003 designs
// the Budgeter to exploit exactly that cache, so the undercount grew as the optimisation worked
// better — an error nobody goes looking for, because the graph improves.
func TestAnthropicCacheTokensReachTheLedger(t *testing.T) {
	newServer := func(t *testing.T, model string, usage map[string]any) *httptest.Server {
		upstream := fakeAnthropicUpstream(t, usage)
		gw := modelgateway.New()
		gw.RegisterProvider(anthropic.Name, &anthropic.Adapter{APIKey: "unused", BaseURL: upstream.URL})

		s := newAPITestStore(t)
		pricing := finops.NewPricingTable([]finops.Rate{
			{Provider: anthropic.Name, Model: model, CostModel: "token_based", InputPerMillionUSD: 10, OutputPerMillionUSD: 30},
		})
		mux := http.NewServeMux()
		(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: s.FinOpsLedger()}).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	}

	decide := func(t *testing.T, srv *httptest.Server, model string) map[string]any {
		t.Helper()
		status, body := postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: anthropic.Name, Model: model, Priority: 0}},
			RenderedContext: map[string]any{"messages": []any{}},
		})
		if status != http.StatusOK {
			t.Fatalf("POST /decide: status=%d body=%v", status, body)
		}
		// /decide wraps the adapter's normalized response under "output" — the usage block lives
		// there, not at the top level, which is where the cost fields sit.
		output, _ := body["output"].(map[string]any)
		usage, _ := output["usage"].(map[string]any)
		if usage == nil {
			t.Fatalf("no usage block in the response: %v", body)
		}
		return usage
	}

	t.Run("cached input is counted, and counted once", func(t *testing.T) {
		// Anthropic reports input_tokens EXCLUDING cached reads, so the inclusive convention agreed
		// with Synaptum and Axonium means this adapter has to add: 100 + 900 = 1000 total input, of
		// which 900 came from cache. Reading input_tokens alone reported 100 and silently dropped
		// nine tenths of the real input.
		model := "claude-cache-test"
		srv := newServer(t, model, map[string]any{
			"input_tokens": 100, "output_tokens": 50,
			"cache_read_input_tokens": 900, "cache_creation_input_tokens": 20,
		})

		usage := decide(t, srv, model)
		if got := toInt(usage["prompt_tokens"]); got != 1000 {
			t.Errorf("prompt_tokens = %d, want 1000 (input must be the TOTAL, cached included)", got)
		}
		if got := toInt(usage["cache_read_tokens"]); got != 900 {
			t.Errorf("cache_read_tokens = %d, want 900", got)
		}
		// cache_write is not part of input, by the same agreement — it must be reported without
		// being folded into prompt_tokens.
		if got := toInt(usage["cache_write_tokens"]); got != 20 {
			t.Errorf("cache_write_tokens = %d, want 20", got)
		}
		if got := toInt(usage["total_tokens"]); got != 1050 {
			t.Errorf("total_tokens = %d, want 1050 (input total + output, cache_write excluded)", got)
		}
	})

	t.Run("a response with no cache accounting reports nothing, not zero", func(t *testing.T) {
		// Axonium hit this exact bug one level down and it is worth not repeating: a missing counter
		// read as 0 turns "this backend does not measure caching" into "the cache was cold". One is
		// an absence of data, the other is data.
		model := "claude-cache-test"
		srv := newServer(t, model, map[string]any{"input_tokens": 100, "output_tokens": 50})

		usage := decide(t, srv, model)
		if got := toInt(usage["prompt_tokens"]); got != 100 {
			t.Errorf("prompt_tokens = %d, want 100", got)
		}
		if _, present := usage["cache_read_tokens"]; present {
			t.Errorf("cache_read_tokens is present (%v) for a provider that reported no cache accounting", usage["cache_read_tokens"])
		}
		if _, present := usage["cache_write_tokens"]; present {
			t.Errorf("cache_write_tokens is present (%v) for a provider that reported no cache accounting", usage["cache_write_tokens"])
		}
	})

	t.Run("a reported zero is kept as a zero", func(t *testing.T) {
		// The other half of the same distinction: the provider did measure, and measured nothing.
		model := "claude-cache-test"
		srv := newServer(t, model, map[string]any{
			"input_tokens": 100, "output_tokens": 50, "cache_read_input_tokens": 0,
		})

		usage := decide(t, srv, model)
		v, present := usage["cache_read_tokens"]
		if !present {
			t.Fatal("cache_read_tokens is absent for a provider that reported 0 — a measured zero is data")
		}
		if toInt(v) != 0 {
			t.Errorf("cache_read_tokens = %v, want 0", v)
		}
	})

	t.Run("the cached tokens are durably recorded, not just returned", func(t *testing.T) {
		// The response could be right while the ledger stayed blind, which is precisely the state
		// this feature found the system in.
		model := fmt.Sprintf("claude-cache-ledger-%s", randSuffix(t))
		srv := newServer(t, model, map[string]any{
			"input_tokens": 100, "output_tokens": 50,
			"cache_read_input_tokens": 900, "cache_creation_input_tokens": 20,
		})
		decide(t, srv, model)

		s := newAPITestStore(t)
		totals, err := s.FinOpsLedger().TotalsByModel(context.Background())
		if err != nil {
			t.Fatalf("totals: %v", err)
		}
		var found bool
		for _, row := range totals {
			if row.Model != model {
				continue
			}
			found = true
			if row.TotalPromptTokens != 1000 {
				t.Errorf("ledger total_prompt_tokens = %d, want 1000", row.TotalPromptTokens)
			}
			if row.TotalCacheReadTokens != 900 {
				t.Errorf("ledger total_cache_read_tokens = %d, want 900", row.TotalCacheReadTokens)
			}
			if row.TotalCacheWriteTokens != 20 {
				t.Errorf("ledger total_cache_write_tokens = %d, want 20", row.TotalCacheWriteTokens)
			}
		}
		if !found {
			t.Fatalf("no ledger row for model %s", model)
		}
	})
}
