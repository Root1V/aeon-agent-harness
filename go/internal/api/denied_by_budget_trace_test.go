package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
)

// TestABudgetStopSaysItWasTheBudget is OBS-020's first half, against real Tempo.
//
// `argus.denied_by` has three values and only `policy` had a producer. A spend ceiling is a refusal
// too — and it was not merely unattributed, it was ABSENT: this handler creates no span on the happy
// path (the gateway makes one per candidate it actually tries), so a request stopped BEFORE routing
// left no trace of having been stopped at all. The ceiling is the one thing in this system that
// costs money when it fails silently, which is why MDL-018 exists.
func TestABudgetStopSaysItWasTheBudget(t *testing.T) {
	tempoURL := os.Getenv("AEON_TEST_TEMPO_QUERY_URL")
	if tempoURL == "" {
		t.Skip("AEON_TEST_TEMPO_QUERY_URL not set — skipping tracing integration test (see make test-go-integration)")
	}
	flush := ensureTestTracing(t)
	ctx := context.Background()
	s := newAPITestStore(t)

	model := "obs020-model-" + randSuffix(t)
	const perCall = 0.025
	pricing := finops.NewPricingTable([]finops.Rate{
		{Provider: "fake-token", Model: model, CostModel: "token_based", InputPerMillionUSD: 10, OutputPerMillionUSD: 30},
	})
	gw := modelgateway.New()
	gw.RegisterProvider("fake-token", &finOpsFakeProvider{costModel: "token_based"})

	// A ceiling of ONE call's worth, so the second request is refused before routing.
	agentName := "obs020-agent-" + randSuffix(t)
	agentRef := agentName + "@0.1.0"
	if _, err := s.AgentRegistryFor("default").Create(ctx, map[string]any{
		"apiVersion": "harness.ai/v1", "kind": "Agent",
		"metadata": map[string]any{"name": agentName, "version": "0.1.0", "owner": "test", "lifecycle": "Draft"},
		"spec":     map[string]any{"runtime": map[string]any{"budgets": map[string]any{"costUsd": perCall}}},
	}, "test"); err != nil {
		t.Fatalf("registering the agent: %v", err)
	}

	mux := http.NewServeMux()
	(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: s, Agents: s}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux, agentRef))
	t.Cleanup(srv.Close)

	runID := "obs020-run-" + randSuffix(t)
	decide := func() (int, map[string]any) {
		return postDecide(t, srv, decideRequest{
			Candidates:       []decideCandidate{{Provider: "fake-token", Model: model, Priority: 0}},
			RenderedContext:  map[string]any{"messages": []any{}, "model": model},
			RunID:            runID,
			AgentManifestRef: agentRef,
		})
	}

	if status, body := decide(); status != http.StatusOK {
		t.Fatalf("the first call was refused (%d): the ceiling must allow one. body=%v", status, body)
	}
	if status, _ := decide(); status != http.StatusPaymentRequired {
		t.Fatalf("the second call returned %d, want 402 — this test measures a budget stop", status)
	}

	if err := flush(ctx); err != nil {
		t.Fatalf("flushing spans: %v", err)
	}

	// 1. THE REFUSAL IS IN THE TRACE, with the three things that make it actionable: that it was
	//    refused, by what, and WHICH RUN.
	//
	//    SCOPED BY THE RUN ID AND NOT BY A SAME-TRACE JOIN. The first version of this query used
	//    `&&` to anchor the refusal to this run's chat span, and FAILED measured: the refusal span
	//    starts its own trace here (postDecide propagates no traceparent), so the two are two
	//    traces. That the run id was the right anchor is also what showed the span was missing it —
	//    a budget stop that does not say which run was stopped is close to useless.
	waitForTempoSpan(t, tempoURL, fmt.Sprintf(
		`{ name = "POST /decide" && span.argus.outcome = "denied" && span.argus.denied_by = "budget" && span.argus.run.id = "%s" }`,
		runID), 30*time.Second)

	// 2. CONTROL: THE HAPPY PATH CREATES NO SUCH SPAN. The whole reason this needed no agreement with
	//    Argus about volume is that the span exists only on the refusal — one per budget stop, not
	//    one per request. A `POST /decide` span that was not a denial would break that claim.
	if n, err := tempoSearchCount(t, tempoURL, fmt.Sprintf(
		`{ name = "POST /decide" && span.argus.run.id = "%s" && span.argus.denied_by != "budget" }`,
		runID)); err != nil || n != 0 {
		t.Fatalf("a POST /decide span exists that is not a budget denial (matched=%d err=%v) — this route "+
			"must produce a span ONLY when it refuses", n, err)
	}
}
