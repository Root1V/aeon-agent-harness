package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// TestARunIsStoppedWhenItsManifestSaysEnough is MDL-017's acceptance test.
//
// WHAT WAS DECLARED AND NOT ENFORCED: examples/deep-research/agent.yaml has carried
// `spec.runtime.budgets: {modelCalls: 60, costUsd: 5.0}` since it was written, and `costUsd` had ZERO
// references in the Go tree. RUN-003 enforces tool calls, depth and wall-clock in the Graph Runtime,
// and its own docstring says model_calls/tokens/cost_usd are not enforced. So a loop that stayed inside
// its tool-call budget could spend without limit, which is the one remaining thing I would not hand to
// another team.
//
// THE CEILING COMES FROM THE MANIFEST AND NOT THE REQUEST, which is the design decision: a caller that
// declares its own ceiling can raise it. The manifest is what a release gate checks and an auditor
// reads, and the gateway reads it from the registry it already has a connection to.
//
// Real Postgres, the real registry, the real ledger and the real /decide handler: the property is about
// what the gateway does after real rows exist, and a fake ledger would assert the shape of a call.
func TestARunIsStoppedWhenItsManifestSaysEnough(t *testing.T) {
	ctx := context.Background()
	s := newAPITestStore(t)
	ledgerStore := s
	ledger := s.FinOpsLedgerFor("default")
	agentsStore := s
	agents := s.AgentRegistryFor("default")

	model := "ceiling-test-model-" + randSuffix(t)
	// $0.025 a call: 1000 prompt + 500 completion at $10/$30 per million (finOpsFakeProvider).
	const perCall = 0.025
	pricing := finops.NewPricingTable([]finops.Rate{
		{Provider: "fake-token", Model: model, CostModel: "token_based", InputPerMillionUSD: 10, OutputPerMillionUSD: 30},
	})
	gw := modelgateway.New()
	gw.RegisterProvider("fake-token", &finOpsFakeProvider{costModel: "token_based"})

	// An agent whose manifest allows two calls' worth of spend and nothing more.
	agentName := "ceiling-agent-" + randSuffix(t)
	agentRef := agentName + "@0.1.0"
	if _, err := agents.Create(ctx, map[string]any{
		"apiVersion": "harness.ai/v1",
		"kind":       "Agent",
		"metadata":   map[string]any{"name": agentName, "version": "0.1.0", "owner": "test", "lifecycle": "Draft"},
		"spec":       map[string]any{"runtime": map[string]any{"budgets": map[string]any{"costUsd": 2 * perCall}}},
	}, "test"); err != nil {
		t.Fatalf("registering the agent: %v", err)
	}

	mux := http.NewServeMux()
	(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledgerStore, Agents: agentsStore}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux, agentRef))
	t.Cleanup(srv.Close)

	decide := func(t *testing.T, runID string) (int, map[string]any) {
		t.Helper()
		return postDecide(t, srv, decideRequest{
			Candidates:       []decideCandidate{{Provider: "fake-token", Model: model, Priority: 0}},
			RenderedContext:  map[string]any{"messages": []any{}},
			RunID:            runID,
			AgentManifestRef: agentRef,
		})
	}

	t.Run("a run spends up to its ceiling and is then refused", func(t *testing.T) {
		runID := "ceiling-run-" + randSuffix(t)
		// The ceiling is checked against spend RECORDED SO FAR, so the call that crosses it is allowed
		// and the next one is not. Two calls reach $0.05, which is the ceiling.
		for i := 1; i <= 2; i++ {
			if status, body := decide(t, runID); status != http.StatusOK {
				t.Fatalf("call %d was refused before the ceiling was reached: status=%d body=%v", i, status, body)
			}
		}
		status, body := decide(t, runID)
		if status != http.StatusPaymentRequired {
			t.Fatalf("the third call returned %d, want 402 — the run has spent its whole ceiling and the "+
				"budget is the one thing that must stop it: %v", status, body)
		}
		if body["retryable"] != false {
			t.Fatalf("the refusal is not marked non-retryable (%v). A retried budget stop is a loop that "+
				"spends its remaining time asking for a call that can never be allowed", body["retryable"])
		}
		// The numbers, because a refusal that will not say how much was spent sends an operator to the
		// database to find out whether the cap is right.
		for _, key := range []string{"spent_usd", "ceiling", "run_id"} {
			if _, ok := body[key]; !ok {
				t.Fatalf("the refusal does not report %q: %v", key, body)
			}
		}
	})

	t.Run("a different run is unaffected", func(t *testing.T) {
		// The sum is per run, which is what makes the ceiling a per-run budget rather than a per-agent
		// lifetime one. If this failed, one finished run would have poisoned the agent forever.
		if status, body := decide(t, "ceiling-fresh-"+randSuffix(t)); status != http.StatusOK {
			t.Fatalf("a run with no spend was refused: status=%d body=%v", status, body)
		}
	})

	t.Run("a call naming no run is not capped", func(t *testing.T) {
		// Legitimate — a script, an eval — and there is nothing to sum. The alternative is refusing every
		// call that is not part of a run, which would break the eval harness to enforce a budget nobody
		// declared for it.
		if status, body := decide(t, ""); status != http.StatusOK {
			t.Fatalf("a call outside any run was refused: status=%d body=%v", status, body)
		}
	})

	t.Run("an agent that declares no ceiling is not capped at zero", func(t *testing.T) {
		// ABSENT IS NOT ZERO, and getting this wrong would refuse the first call of every run whose
		// manifest has no budgets block — which is most of them.
		otherName := "no-ceiling-agent-" + randSuffix(t)
		otherRef := otherName + "@0.1.0"
		if _, err := agents.Create(ctx, map[string]any{
			"apiVersion": "harness.ai/v1", "kind": "Agent",
			"metadata": map[string]any{"name": otherName, "version": "0.1.0", "owner": "test", "lifecycle": "Draft"},
			"spec":     map[string]any{"runtime": map[string]any{"maxTurns": 10}},
		}, "test"); err != nil {
			t.Fatalf("registering: %v", err)
		}
		mux2 := http.NewServeMux()
		(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledgerStore, Agents: agentsStore}).Register(mux2)
		srv2 := httptest.NewServer(authWrap(t, mux2, otherRef))
		t.Cleanup(srv2.Close)

		runID := "no-ceiling-run-" + randSuffix(t)
		for i := 1; i <= 3; i++ {
			status, body := postDecide(t, srv2, decideRequest{
				Candidates:       []decideCandidate{{Provider: "fake-token", Model: model, Priority: 0}},
				RenderedContext:  map[string]any{"messages": []any{}},
				RunID:            runID,
				AgentManifestRef: otherRef,
			})
			if status != http.StatusOK {
				t.Fatalf("call %d to an agent with no declared ceiling returned %d: %v", i, status, body)
			}
		}
	})

	t.Run("unpriced spend is reported, not counted as zero", func(t *testing.T) {
		// OBS-009's rule, already settled for the circuit breaker: what is measured fires on real spend
		// however much unpriced traffic sits beside it, and the unpriced count travels so the reader knows
		// the figure is a lower bound. Here the run is pushed over with priced calls AND has an unpriced
		// one, so the refusal must mention both.
		unpricedModel := "ceiling-unpriced-" + randSuffix(t)
		gw.RegisterProvider("fake-compute", &finOpsFakeProvider{costModel: "compute_based"})
		runID := "ceiling-mixed-" + randSuffix(t)

		// One call with no configured rate: recorded with a NULL cost (OBS-008), not a zero.
		if status, body := postDecide(t, srv, decideRequest{
			Candidates:       []decideCandidate{{Provider: "fake-compute", Model: unpricedModel, Priority: 0}},
			RenderedContext:  map[string]any{"messages": []any{}},
			RunID:            runID,
			AgentManifestRef: agentRef,
		}); status != http.StatusOK {
			t.Fatalf("the unpriced call was refused: status=%d body=%v", status, body)
		}
		for i := 1; i <= 2; i++ {
			if status, _ := decide(t, runID); status != http.StatusOK {
				t.Fatalf("priced call %d was refused early", i)
			}
		}
		status, body := decide(t, runID)
		if status != http.StatusPaymentRequired {
			t.Fatalf("status = %d, want 402: %v", status, body)
		}
		if body["unpriced_calls"] == nil {
			t.Fatalf("the refusal does not mention the unpriced call, so the reported spend reads as exact "+
				"when it is a lower bound: %v", body)
		}
	})

	t.Run("and the ledger's own sum is what was enforced", func(t *testing.T) {
		// The number enforced has to be the number an operator sees on the FinOps dashboard, which is
		// why this reads the ledger rather than an in-process counter: a counter would reset on a deploy
		// and differ per replica.
		runID := "ceiling-sum-" + randSuffix(t)
		if status, _ := decide(t, runID); status != http.StatusOK {
			t.Fatalf("the first call was refused")
		}
		spend, err := ledger.SpendForRun(ctx, runID)
		if err != nil {
			t.Fatalf("SpendForRun: %v", err)
		}
		if diff := spend.CostUSD - perCall; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("SpendForRun = %v, want %v", spend.CostUSD, perCall)
		}
		if spend.ModelCalls != 1 {
			t.Fatalf("model_calls = %d, want 1", spend.ModelCalls)
		}
	})
}

// TestATokenCeilingStopsARunThatIsNeverPriced is MDL-018's half, and it is not redundant with the
// cost ceiling.
//
// WHY A SECOND CEILING. A deployment on local inference is never priced: `prometheus_inference` is
// compute_based, so OBS-008 records its rows with a NULL cost on purpose. A cost ceiling therefore
// cannot cap it — the sum stays 0.0 forever and the run spends GPU time without limit. Tokens are
// reported by every provider that reports usage at all, priced or not, so the token ceiling is the one
// that works there.
func TestATokenCeilingStopsARunThatIsNeverPriced(t *testing.T) {
	ctx := context.Background()
	s := newAPITestStore(t)
	ledgerStore := s
	ledger := s.FinOpsLedgerFor("default")
	agentsStore := s
	agents := s.AgentRegistryFor("default")

	// A compute_based model: real token usage, NO price. finOpsFakeProvider reports 1000 + 500.
	model := "token-ceiling-" + randSuffix(t)
	pricing := finops.NewPricingTable([]finops.Rate{
		{Provider: "fake-compute", Model: model, CostModel: "compute_based"},
	})
	gw := modelgateway.New()
	gw.RegisterProvider("fake-compute", &finOpsFakeProvider{costModel: "compute_based"})

	agentName := "token-ceiling-agent-" + randSuffix(t)
	agentRef := agentName + "@0.1.0"
	if _, err := agents.Create(ctx, map[string]any{
		"apiVersion": "harness.ai/v1", "kind": "Agent",
		"metadata": map[string]any{"name": agentName, "version": "0.1.0", "owner": "test", "lifecycle": "Draft"},
		// Two calls' worth: 1500 tokens each. A cost ceiling is deliberately NOT declared, so if this
		// test passes it is the token ceiling doing the work.
		"spec": map[string]any{"runtime": map[string]any{"budgets": map[string]any{"tokens": 3000}}},
	}, "test"); err != nil {
		t.Fatalf("registering the agent: %v", err)
	}

	mux := http.NewServeMux()
	(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledgerStore, Agents: agentsStore}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux, agentRef))
	t.Cleanup(srv.Close)

	runID := "token-run-" + randSuffix(t)
	call := func() (int, map[string]any) {
		return postDecide(t, srv, decideRequest{
			Candidates:       []decideCandidate{{Provider: "fake-compute", Model: model, Priority: 0}},
			RenderedContext:  map[string]any{"messages": []any{}},
			RunID:            runID,
			AgentManifestRef: agentRef,
		})
	}

	for i := 1; i <= 2; i++ {
		if status, body := call(); status != http.StatusOK {
			t.Fatalf("call %d was refused before the ceiling: status=%d body=%v", i, status, body)
		}
	}
	status, body := call()
	if status != http.StatusPaymentRequired {
		t.Fatalf("the third call returned %d, want 402 at 3000 tokens: %v", status, body)
	}
	if body["spent_usd"] != 0.0 {
		t.Fatalf("spent_usd = %v. The point of this test is that it is zero and the run is stopped "+
			"anyway: a cost ceiling cannot cap a deployment that is never priced", body["spent_usd"])
	}
	if tokens, _ := body["tokens"].(float64); tokens < 3000 {
		t.Fatalf("the refusal reports %v tokens, want at least the ceiling: %v", tokens, body)
	}
	if body["unpriced_calls"] == nil {
		t.Fatalf("three compute_based calls and the refusal does not say the cost is a lower bound: %v", body)
	}

	spend, err := ledger.SpendForRun(ctx, runID)
	if err != nil {
		t.Fatalf("SpendForRun: %v", err)
	}
	// 3000 and not 4500: the third call was REFUSED, so it never reached the provider and left no row.
	// My first version of this assertion expected three calls' worth, which would only be true if the
	// ceiling were a receipt rather than a cap — the number is the proof that nothing was spent on the
	// call that crossed the line.
	if spend.Tokens != 3000 {
		t.Fatalf("ledger tokens = %d, want 3000 (the two calls that were allowed, at 1500 each) — a higher "+
			"number would mean the refused call still reached the provider", spend.Tokens)
	}
}

// TestRunStatusStopsReportingZeroCostForRunsThatSpent is MDL-018's other half, and it is a defect in
// an API a user reads rather than in an internal counter.
//
// `budgets_consumed` reported `model_calls`, `tokens` and `cost_usd` as 0 on EVERY run: the generic
// Graph Runtime has no model_call node kind and never increments them, and GET /runs/{id} returned the
// dict verbatim. So a run that had spent two dollars answered `cost_usd: 0.0` — "this run was free".
// It is OBS-008's `DEFAULT 0` sitting in a user-facing endpoint, and the fix is the same: report what
// was measured, and omit what was not.
//
// Asserted against the ledger rather than against a fixture, because the point is that the number a
// user sees is the number the FinOps dashboard shows and the ceiling is enforced against.
func TestRunStatusStopsReportingZeroCostForRunsThatSpent(t *testing.T) {
	ctx := context.Background()
	s := newAPITestStore(t)
	ledger := s.FinOpsLedgerFor("default")

	runID := "status-spend-" + randSuffix(t)
	cost := 1.25
	for i := 0; i < 2; i++ {
		if err := ledger.Record(ctx, store.CostEntry{
			Provider: "fake-token", Model: "status-model", CostModel: strPtr("token_based"),
			PromptTokens: intPtr(1000), CompletionTokens: intPtr(500), CostUSD: &cost, RunID: runID,
		}); err != nil {
			t.Fatalf("recording spend: %v", err)
		}
	}

	spend, err := ledger.SpendForRun(ctx, runID)
	if err != nil {
		t.Fatalf("SpendForRun: %v", err)
	}
	if diff := spend.CostUSD - 2*cost; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %v, want %v", spend.CostUSD, 2*cost)
	}
	if spend.Tokens != 3000 {
		t.Fatalf("tokens = %d, want 3000", spend.Tokens)
	}
	if spend.ModelCalls != 2 {
		t.Fatalf("model_calls = %d, want 2", spend.ModelCalls)
	}

	// And the workflow no longer answers the three keys at all, which is what lets the Run Controller
	// fill them. A workflow that still returned zeros would overwrite the real numbers or be overwritten
	// by them depending on merge order — and either way one of the two would be silently discarded.
	t.Run("the workflow reports only what it counts", func(t *testing.T) {
		for _, fabricated := range []string{"cost_usd", "tokens", "model_calls"} {
			if bytes.Contains(graphRunSource(t), []byte(`"`+fabricated+`": c.`)) {
				t.Fatalf("graph_run.py still returns %q from budgets_consumed. The Graph Runtime never "+
					"increments it, so the value is a fabricated zero presented as a measurement", fabricated)
			}
		}
	})
}

// graphRunSource reads the workflow that answers the budgets_consumed query.
//
// Asserted on the SOURCE because the alternative is a real Temporal server and a real worker for a
// property about which keys a query returns — and because the defect was a literal in that function,
// so the literal is the honest thing to check.
func graphRunSource(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(filepath.Dir(repoPolicyBundlePath(t)), "..", "..", "python", "aeon_worker", "workflows", "graph_run.py")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return raw
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }
