package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/providers"
)

// unusableAnswerProvider answers 200 with NO content and NO tool calls, and with real usage.
//
// This is MDL-015's case, measured on the real deployment back then: gpt-oss-20b-mxfp4 answers a
// Researcher prompt with finish_reason "stop", a short reasoning_content and content "" — everything
// went to its analysis channel. The gateway treats that as this candidate failing and moves on, which
// is what the cascade is for. What it did NOT do is record that the generation happened.
type unusableAnswerProvider struct{ costModel string }

func (p *unusableAnswerProvider) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	model, _ := renderedContext["model"].(string)
	// Reasoning but no answer: usable by nobody, paid for in full.
	out := providers.NormalizedChatResponse(model, "", "stop", tokens(1000), tokens(500))
	choices, _ := out["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	message["reasoning_content"] = "thinking, and never answering"
	return out, nil
}
func (p *unusableAnswerProvider) CachingCapability() string { return "none" }
func (p *unusableAnswerProvider) CostModel() string         { return p.costModel }

// FIN-003: a candidate discarded AFTER the model answered leaves a ledger row.
//
// THE DIRECTION OF THE OLD ERROR IS WHY THIS IS A DEFECT AND NOT AN IMPRECISION. The discarded
// generation was billed, and it left no row — so `SpendForRun` under-counted, and a budget ceiling
// that counts low lets through calls it should refuse. Nobody notices a limit that fails to fire.
//
// Raised by two teams in the same week from different directions: Argus, because their FinOps agent
// will read `model_gateway_costs` as the truth about cost, and Prometheus about a disconnected
// client — "a spike of 5xx is not the only way to waste compute".
func TestADiscardedAttemptIsStillBilled(t *testing.T) {
	ctx := context.Background()
	s := newAPITestStore(t)
	ledger := s.FinOpsLedgerFor("default")

	badModel := "fin003-unusable-" + randSuffix(t)
	goodModel := "fin003-good-" + randSuffix(t)
	pricing := finops.NewPricingTable([]finops.Rate{
		{Provider: "fake-unusable", Model: badModel, CostModel: "token_based", InputPerMillionUSD: 10, OutputPerMillionUSD: 30},
		{Provider: "fake-token", Model: goodModel, CostModel: "token_based", InputPerMillionUSD: 10, OutputPerMillionUSD: 30},
	})
	gw := modelgateway.New()
	gw.RegisterProvider("fake-unusable", &unusableAnswerProvider{costModel: "token_based"})
	gw.RegisterProvider("fake-token", &finOpsFakeProvider{costModel: "token_based"})

	mux := http.NewServeMux()
	(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: s, Agents: s}).Register(mux)
	(&FinOpsHandlers{Ledger: s}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)

	// Both candidates answer. The first one's answer is unusable, so the second serves — and BOTH
	// generations happened at 1000/500 tokens, $0.025 each.
	runID := "fin003-run-" + randSuffix(t)
	status, body := postDecide(t, srv, decideRequest{
		Candidates: []decideCandidate{
			{Provider: "fake-unusable", Model: badModel, Priority: 0},
			{Provider: "fake-token", Model: goodModel, Priority: 1},
		},
		RenderedContext: map[string]any{"messages": []any{}},
		RunID:           runID,
	})
	if status != http.StatusOK {
		t.Fatalf("POST /decide: status=%d body=%v", status, body)
	}
	if body["provider_used"] != "fake-token" {
		t.Fatalf("provider_used = %v, want the second candidate to have served", body["provider_used"])
	}

	spend, err := ledger.SpendForRun(ctx, runID)
	if err != nil {
		t.Fatalf("SpendForRun: %v", err)
	}
	if spend.DiscardedCalls != 1 {
		t.Fatalf("discarded_calls = %d, want 1 — the first candidate generated an answer, was billed "+
			"for it, and left no trace before FIN-003", spend.DiscardedCalls)
	}
	if spend.ModelCalls != 1 {
		t.Errorf("model_calls = %d, want 1: a discarded attempt is not a call anyone was served, and "+
			"counting it here would make the average cost per call describe work that did not happen",
			spend.ModelCalls)
	}
	// $0.025 served + $0.025 discarded. The CEILING reads this number.
	if !closeEnough(spend.CostUSD, 0.05) {
		t.Errorf("cost_usd = %v, want 0.05 — the money left for both generations", spend.CostUSD)
	}
	if !closeEnough(spend.DiscardedCostUSD, 0.025) {
		t.Errorf("discarded_cost_usd = %v, want 0.025", spend.DiscardedCostUSD)
	}
	if spend.Tokens != 3000 || spend.PromptTokens != 2000 || spend.CompletionTokens != 1000 {
		t.Errorf("tokens = %d (%d in / %d out), want 3000 (2000/1000)", spend.Tokens, spend.PromptTokens, spend.CompletionTokens)
	}

	t.Run("the route and the dashboard agree about it", func(t *testing.T) {
		got := getRunCost(t, srv, runID, http.StatusOK)
		if got.DiscardedCalls != 1 || !closeEnough(got.DiscardedCostUSD, 0.025) {
			t.Errorf("the JSON reports discarded_calls=%d discarded_cost_usd=%v, want 1 / 0.025",
				got.DiscardedCalls, got.DiscardedCostUSD)
		}
		if got.ModelCalls != 1 {
			t.Errorf("model_calls = %d over the wire, want 1 — the meaning announced to Veritium in "+
				"FIN-002 must not change under them", got.ModelCalls)
		}
		if got.CostUSD == nil || !closeEnough(*got.CostUSD, 0.05) {
			t.Errorf("cost_usd = %v over the wire, want 0.05", jsonCost(got.CostUSD))
		}
		page := fetchDashboard(t, srv)
		row := runRowFromDashboard(t, page, runID)
		if row.calls != got.ModelCalls {
			t.Errorf("the HTML says %d calls and the JSON says %d", row.calls, got.ModelCalls)
		}
		if !closeEnough(row.costUSD, *got.CostUSD) {
			t.Errorf("the HTML says $%.4f and the JSON says %v", row.costUSD, jsonCost(got.CostUSD))
		}
		if !strings.Contains(page, "generated, billed and discarded") {
			t.Error("the dashboard does not say that part of its total paid for answers nobody received")
		}
	})

	t.Run("a 502 with every candidate unusable still bills what was generated", func(t *testing.T) {
		// THE WORST VERSION OF THE HOLE, and the reason the gateway's failure is now a typed error: every
		// candidate answered, every answer was unusable, every generation was billed, and the run's
		// recorded spend was zero.
		lostRun := "fin003-all-unusable-" + randSuffix(t)
		status, body := postDecide(t, srv, decideRequest{
			Candidates: []decideCandidate{
				{Provider: "fake-unusable", Model: badModel, Priority: 0},
				{Provider: "fake-unusable", Model: badModel, Priority: 1},
			},
			RenderedContext: map[string]any{"messages": []any{}},
			RunID:           lostRun,
		})
		if status != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 when no candidate produced a usable answer: %v", status, body)
		}
		spend, err := ledger.SpendForRun(ctx, lostRun)
		if err != nil {
			t.Fatalf("SpendForRun: %v", err)
		}
		if spend.DiscardedCalls != 2 {
			t.Fatalf("discarded_calls = %d after a 502, want 2: the caller got an error and the models "+
				"still ran twice", spend.DiscardedCalls)
		}
		if spend.ModelCalls != 0 {
			t.Errorf("model_calls = %d, want 0 — nobody was served", spend.ModelCalls)
		}
		if !closeEnough(spend.CostUSD, 0.05) {
			t.Errorf("cost_usd = %v after a 502, want 0.05", spend.CostUSD)
		}
		// And the run EXISTS as far as the cost route is concerned, even though it has no checkpoint and
		// no served call: it spent money, which is the thing that route answers about.
		got := getRunCost(t, srv, lostRun, http.StatusOK)
		if got.DiscardedCalls != 2 {
			t.Errorf("the route reports discarded_calls=%d for a run whose only trace is discarded "+
				"spend, want 2", got.DiscardedCalls)
		}
	})

	t.Run("an attempt that never reached a model leaves no row", func(t *testing.T) {
		// THE LINE BETWEEN RECORDING SPEND AND INVENTING ROWS. A candidate that failed before producing
		// anything cost nothing, and a cost ledger is not an attempt log — that failure is on its span,
		// which is where it belongs.
		noProviderRun := "fin003-unregistered-" + randSuffix(t)
		status, _ := postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: "not-registered-at-all", Model: "x", Priority: 0}},
			RenderedContext: map[string]any{"messages": []any{}},
			RunID:           noProviderRun,
		})
		if status != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", status)
		}
		spend, err := ledger.SpendForRun(ctx, noProviderRun)
		if err != nil {
			t.Fatalf("SpendForRun: %v", err)
		}
		if spend.DiscardedCalls != 0 || spend.ModelCalls != 0 {
			t.Errorf("the ledger holds %d served and %d discarded rows for a run whose candidate was "+
				"never even dialled", spend.ModelCalls, spend.DiscardedCalls)
		}
	})

	t.Run("the budget ceiling sees the discarded spend", func(t *testing.T) {
		// THE WHOLE POINT, stated as the behaviour that was wrong: a ceiling that only counted served
		// calls let a run keep spending past its limit, and the limit's silence looked like compliance.
		//
		// Ceiling 1500 tokens. ONE discarded attempt is 1500 tokens, so the next call must be refused —
		// and before FIN-003 the ledger would have said this run had spent nothing at all.
		agentName := "fin003-agent-" + randSuffix(t)
		agentRef := agentName + "@0.1.0"
		if _, err := s.AgentRegistryFor("default").Create(ctx, map[string]any{
			"apiVersion": "harness.ai/v1", "kind": "Agent",
			"metadata": map[string]any{"name": agentName, "version": "0.1.0", "owner": "test", "lifecycle": "Draft"},
			"spec":     map[string]any{"runtime": map[string]any{"budgets": map[string]any{"tokens": 1500}}},
		}, "test"); err != nil {
			t.Fatalf("registering the agent: %v", err)
		}
		cappedRun := "fin003-capped-" + randSuffix(t)

		// First call: the only candidate answers unusably, so this is a 502 — and 1500 tokens are spent.
		status, _ := postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: "fake-unusable", Model: badModel, Priority: 0}},
			RenderedContext: map[string]any{"messages": []any{}},
			RunID:           cappedRun, AgentManifestRef: agentRef,
		})
		if status != http.StatusBadGateway {
			t.Fatalf("first call: status = %d, want 502", status)
		}

		// Second call: a candidate that WOULD work. It must be refused anyway, because the budget is
		// already spent — on an answer nobody received.
		status, body := postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: "fake-token", Model: goodModel, Priority: 0}},
			RenderedContext: map[string]any{"messages": []any{}},
			RunID:           cappedRun, AgentManifestRef: agentRef,
		})
		if status != http.StatusPaymentRequired {
			t.Fatalf("second call: status = %d, want 402 — the 1500-token ceiling was already consumed "+
				"by a discarded generation, and a ceiling that cannot see it never fires: %v", status, body)
		}
	})
}
