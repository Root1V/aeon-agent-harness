package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	openaicompatible "github.com/aeon-ai/aeon/go/internal/providers/openai_compatible"
)

// TestUnreportedUsageIsNotRecordedAsZero is MDL-014's acceptance test.
//
// A response with no `usage` object normalized to prompt_tokens: 0, and the ledger recorded a 0-token,
// $0 call for it. Same fabricated fact MDL-012 removed from the cache counters and OBS-008 removed from
// cost_usd, on the two counters everything reads.
//
// Found by RUNNING Synaptum's normalization corpus rather than by reading code, and it was the last
// real divergence of its ten cases — the corpus's knownDivergences map is now empty, and a new guard
// there fails if an entry stops diverging, so it cannot rot back into an excuse.
//
// The roadmap said this should land with FND-004's five-counter shape "y no suelto". The repo's own
// history says otherwise and that is why it landed alone: CacheReadTokens, CacheWriteTokens and
// ReasoningTokens were each made nullable one feature at a time (MDL-012, MDL-016). Three of five were
// already pointers, so this completes a pattern rather than pre-empting a contract.
func TestUnreportedUsageIsNotRecordedAsZero(t *testing.T) {
	ctx := context.Background()

	// upstream serves a chat response whose usage block is whatever the test hands it — including
	// nothing at all, which is the case under test and one no fixture had covered.
	upstream := func(t *testing.T, usage map[string]any) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := map[string]any{
				"model": "m",
				"choices": []any{map[string]any{
					"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": "hola"},
				}},
			}
			if usage != nil {
				body["usage"] = usage
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	run := func(t *testing.T, usage map[string]any) (map[string]any, string) {
		t.Helper()
		model := "mdl014-" + randSuffix(t)
		gw := modelgateway.New()
		gw.RegisterProvider("oc", &openaicompatible.Adapter{BaseURL: upstream(t, usage).URL})
		pricing := finops.NewPricingTable([]finops.Rate{{
			Provider: "oc", Model: model, CostModel: "token_based",
			InputPerMillionUSD: 10, OutputPerMillionUSD: 30,
		}})
		ledger := newAPITestStore(t).FinOpsLedger()
		mux := http.NewServeMux()
		(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledger}).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)

		status, body := postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: "oc", Model: model, Priority: 0}},
			RenderedContext: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		})
		if status != http.StatusOK {
			t.Fatalf("POST /decide: status=%d body=%v", status, body)
		}
		rows, err := ledger.RowsForModel(ctx, "oc", model, 5)
		if err != nil {
			t.Fatalf("RowsForModel: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d ledger rows, want exactly 1: %+v", len(rows), rows)
		}
		if rows[0].PromptTokens == nil {
			return body, "prompt=nil"
		}
		return body, ""
	}

	t.Run("a response with no usage object records no token counts", func(t *testing.T) {
		body, _ := run(t, nil)

		// The normalized response omits what it was not told, rather than saying zero.
		output, _ := body["output"].(map[string]any)
		usage, _ := output["usage"].(map[string]any)
		if _, present := usage["prompt_tokens"]; present {
			t.Errorf("usage.prompt_tokens = %v is present for a response that carried no usage at all — an absent key says 'not measured', a zero claims otherwise", usage["prompt_tokens"])
		}
		if _, present := usage["total_tokens"]; present {
			t.Error("usage.total_tokens is present with no counters to total — a total built from two absent halves is a fabricated number, not a smaller one")
		}

		// And there is no price, because there is nothing to multiply. Unpriced beats a $0 that reads as
		// a measurement (OBS-008).
		if _, present := body["cost_usd"]; present {
			t.Errorf("cost_usd = %v for a call whose token consumption nobody reported", body["cost_usd"])
		}
	})

	t.Run("a measured zero is still recorded as zero", func(t *testing.T) {
		// The half that makes the first subtest mean something. If nil were the only representable state
		// we would have swapped one merged fact for another — and a provider really can answer a prompt
		// of zero billable tokens.
		body, _ := run(t, map[string]any{"prompt_tokens": 0, "completion_tokens": 0})

		output, _ := body["output"].(map[string]any)
		usage, _ := output["usage"].(map[string]any)
		if got, present := usage["prompt_tokens"]; !present || got != float64(0) {
			t.Errorf("usage.prompt_tokens = %v (present=%v), want a recorded 0", got, present)
		}
		if got, present := usage["total_tokens"]; !present || got != float64(0) {
			t.Errorf("usage.total_tokens = %v (present=%v), want 0 — both halves were measured", got, present)
		}
		// Priced, at zero: the provider said zero tokens, and zero tokens times a rate is a real $0.
		if got, present := body["cost_usd"]; !present || got != float64(0) {
			t.Errorf("cost_usd = %v (present=%v), want a computed 0", got, present)
		}
	})

	t.Run("a half-reported usage does not invent the missing half", func(t *testing.T) {
		// The case a pointer on the usage OBJECT alone would miss: the object is there, one counter is
		// not. Inventing the absent one would produce a total smaller than the truth, presented as the
		// whole figure — which is worse than no figure, because it looks complete.
		body, _ := run(t, map[string]any{"prompt_tokens": 12})

		output, _ := body["output"].(map[string]any)
		usage, _ := output["usage"].(map[string]any)
		if got := usage["prompt_tokens"]; got != float64(12) {
			t.Errorf("usage.prompt_tokens = %v, want 12", got)
		}
		if _, present := usage["completion_tokens"]; present {
			t.Error("usage.completion_tokens is present for a usage object that did not carry it")
		}
		if _, present := usage["total_tokens"]; present {
			t.Error("usage.total_tokens is present with only one half known — that number would be a lower bound wearing the shape of a total")
		}
		if _, present := body["cost_usd"]; present {
			t.Errorf("cost_usd = %v computed from one known half", body["cost_usd"])
		}
	})
}
