package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	openaicompatible "github.com/aeon-ai/aeon/go/internal/providers/openai_compatible"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// TestCostLedgerDistinguishesUnpricedFromFree is OBS-008's acceptance test.
//
// Two defects, both measured on 2026-09-19 while answering Axonium's warning that Prometheus had
// changed what model_id means in its usage export. Their warning carried a conditional — "if your
// imputation groups by the model's name, look at it today" — and ours did.
//
//   - Every compute_based call (all of prometheus_inference, the only provider we actually use) was
//     recorded with cost_usd = 0 in a NOT NULL DEFAULT 0 column. "Nobody priced this" and "this cost
//     zero" became the same fact, and the dashboard summed them into $0.00.
//   - A model with no configured rate — a renamed one, most likely — made recordCost return BEFORE
//     the ledger write. Not a null price, which an audit can find and ask about: no row at all.
//     Prometheus hit the same bug in their own rows and at least left a row behind.
//
// These run against real Postgres through the real handler, because the property is about what
// lands in the table and what the aggregation does with it. A fake ledger would assert the shape of
// a call and nothing about either.
func TestCostLedgerDistinguishesUnpricedFromFree(t *testing.T) {
	ctx := context.Background()

	// Every subtest gets its OWN model name and its own pricing table keyed on it. The first draft
	// of this test shared one fixed name, which made it pass on a clean database and fail on the
	// second run against the same one — rows from the earlier run were still there and CallCount
	// came back 3. A test that fails only sometimes is the kind that gets silenced rather than
	// fixed, so the isolation is part of the test, not a convenience.
	newServer := func(t *testing.T, ledger *store.FinOpsLedger, provider, model, costModel string, in, out float64) *httptest.Server {
		t.Helper()
		upstream := fakeChatUpstream(t, model)
		gw := modelgateway.New()
		// The provider name is what the ledger groups by, so it has to be the real one under test;
		// the adapter behind it is the real OpenAI-compatible one against a fake upstream.
		gw.RegisterProvider(provider, &openaicompatible.Adapter{BaseURL: upstream.URL})

		var rates []finops.Rate
		if costModel != "" {
			rates = append(rates, finops.Rate{
				Provider: provider, Model: model, CostModel: costModel,
				InputPerMillionUSD: in, OutputPerMillionUSD: out,
			})
		}
		mux := http.NewServeMux()
		(&ModelGatewayHandlers{Gateway: gw, Pricing: finops.NewPricingTable(rates), Ledger: ledger}).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	}

	decide := func(t *testing.T, srv *httptest.Server, provider, model string) map[string]any {
		t.Helper()
		status, body := postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: provider, Model: model, Priority: 0}},
			RenderedContext: map[string]any{"messages": []any{}},
		})
		if status != http.StatusOK {
			t.Fatalf("POST /decide: status=%d body=%v", status, body)
		}
		return body
	}

	t.Run("a compute_based call is recorded with no cost, not with zero", func(t *testing.T) {
		ledger := newAPITestStore(t).FinOpsLedger()
		// A compute_based rate really is configured — examples/deep-research's bundle declares one
		// — it just carries no per-token price.
		provider, model := "prometheus_inference", "gpt-oss-20b-mxfp4-"+randSuffix(t)

		body := decide(t, newServer(t, ledger, provider, model, "compute_based", 0, 0), provider, model)

		// The /decide response was already honest: it omits cost_usd when nothing was priced.
		if _, present := body["cost_usd"]; present {
			t.Errorf("response carries cost_usd for a compute_based call: %v", body["cost_usd"])
		}
		if body["cost_model"] != "compute_based" {
			t.Errorf("cost_model = %v, want compute_based", body["cost_model"])
		}

		// The ledger was not. This is the row that used to read 0.
		row := findCostRow(t, ledger, provider, model)
		if row.TotalCostUSD != nil {
			t.Fatalf("TotalCostUSD = %v, want nil — a GPU-second bill we cannot compute is not a measured amount", *row.TotalCostUSD)
		}
		if row.CallCount != 1 || row.UnpricedCalls != 1 {
			t.Fatalf("calls = %d, unpriced = %d, want 1 and 1", row.CallCount, row.UnpricedCalls)
		}
	})

	t.Run("a genuinely free call is recorded as zero, and stays distinguishable from it", func(t *testing.T) {
		// This is the half that makes the previous subtest mean something. If "unpriced" were the
		// only representable state we would have swapped one merged fact for another.
		ledger := newAPITestStore(t).FinOpsLedger()
		provider, model := "openai", "free-tier-model-"+randSuffix(t)

		body := decide(t, newServer(t, ledger, provider, model, "token_based", 0, 0), provider, model)
		if body["cost_usd"] != float64(0) {
			t.Errorf("cost_usd = %v, want 0 — this model is priced, and its price is zero", body["cost_usd"])
		}

		row := findCostRow(t, ledger, provider, model)
		if row.TotalCostUSD == nil {
			t.Fatal("TotalCostUSD is nil for a model with a configured $0 rate — a measured zero became unmeasured")
		}
		if *row.TotalCostUSD != 0 {
			t.Fatalf("TotalCostUSD = %v, want 0", *row.TotalCostUSD)
		}
		if row.UnpricedCalls != 0 {
			t.Fatalf("UnpricedCalls = %d, want 0 — this call was priced", row.UnpricedCalls)
		}
	})

	t.Run("a model with no configured rate still leaves a row", func(t *testing.T) {
		// The renamed-model case: recordCost used to return here, before the insert.
		ledger := newAPITestStore(t).FinOpsLedger()
		provider := "prometheus_inference"
		model := "gpt-oss-20b-mxfp4-renamed-" + randSuffix(t)

		// No rate at all for this (provider, model) — the renamed-model case.
		decide(t, newServer(t, ledger, provider, model, "", 0, 0), provider, model)

		row := findCostRow(t, ledger, provider, model)
		if row.CallCount != 1 {
			t.Fatalf("CallCount = %d, want 1 — the call left no row at all, which an audit cannot tell from a call that never happened", row.CallCount)
		}
		if row.TotalCostUSD != nil {
			t.Errorf("TotalCostUSD = %v, want nil", *row.TotalCostUSD)
		}
		if row.CostModel != nil {
			t.Errorf("CostModel = %q, want nil — with no rate we do not know how this model bills", *row.CostModel)
		}
	})

	t.Run("a partially priced group reports its unpriced calls beside the sum", func(t *testing.T) {
		// An aggregate cannot carry the row-level distinction, so the count travels next to it.
		// Without that count the total is a lower bound wearing the shape of an exact figure — the
		// same mistake as the zero, one level up.
		ledger := newAPITestStore(t).FinOpsLedger()
		provider, model := "openai", "partial-"+randSuffix(t)

		priced := 0.30
		tokenBased := "token_based"
		if err := ledger.Record(ctx, store.CostEntry{Provider: provider, Model: model, CostModel: &tokenBased, PromptTokens: 10, CompletionTokens: 5, CostUSD: &priced}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if err := ledger.Record(ctx, store.CostEntry{Provider: provider, Model: model, CostModel: &tokenBased, PromptTokens: 10, CompletionTokens: 5}); err != nil {
			t.Fatalf("Record: %v", err)
		}

		row := findCostRow(t, ledger, provider, model)
		if row.CallCount != 2 {
			t.Fatalf("CallCount = %d, want 2", row.CallCount)
		}
		if row.TotalCostUSD == nil || *row.TotalCostUSD != 0.30 {
			t.Fatalf("TotalCostUSD = %v, want 0.30 — the sum of what was priced", row.TotalCostUSD)
		}
		if row.UnpricedCalls != 1 {
			t.Fatalf("UnpricedCalls = %d, want 1 — otherwise $0.30 reads as the whole cost of two calls", row.UnpricedCalls)
		}
	})

	t.Run("the dashboard never prints a figure where it has none", func(t *testing.T) {
		ledger := newAPITestStore(t).FinOpsLedger()
		computeBased := "compute_based"
		if err := ledger.Record(ctx, store.CostEntry{
			Provider: "prometheus_inference", Model: "dash-" + randSuffix(t),
			CostModel: &computeBased, PromptTokens: 100, CompletionTokens: 20,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}

		mux := http.NewServeMux()
		(&FinOpsHandlers{Ledger: ledger}).Register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/finops/costs", nil))

		page := rec.Body.String()
		if !strings.Contains(page, "&mdash;") {
			t.Error("no em dash on the page — an unpriced group is being rendered as a number")
		}
		if !strings.Contains(page, "whose cost is unknown, not zero") {
			t.Error("the grand total does not disclose that part of the ledger is unpriced")
		}
		if strings.Contains(page, "<p>Total: $0.0000</p>") {
			t.Error("the page still prints $0.0000 as the total over an unpriced ledger — the OBS-008 defect")
		}
	})
}

// fakeChatUpstream serves a real OpenAI-compatible chat response with a real usage block, so the
// tokens the ledger records are parsed by the real adapter rather than handed to it.
func fakeChatUpstream(t *testing.T, model string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "hola"},
			}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// findCostRow returns the aggregation row for (provider, model), failing when there is none — which
// is itself one of the things under test.
func findCostRow(t *testing.T, ledger *store.FinOpsLedger, provider, model string) store.ModelTotal {
	t.Helper()
	totals, err := ledger.TotalsByModel(context.Background())
	if err != nil {
		t.Fatalf("TotalsByModel: %v", err)
	}
	for _, row := range totals {
		if row.Provider == provider && row.Model == model {
			return row
		}
	}
	t.Fatalf("no ledger row for (%s, %s) — the call was not recorded at all", provider, model)
	return store.ModelTotal{}
}
