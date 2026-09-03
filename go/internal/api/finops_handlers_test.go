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

// finOpsFakeProvider returns a real NormalizedChatResponse with known, fixed token usage — enough
// to compute a real, checkable dollar cost from.
type finOpsFakeProvider struct {
	costModel string
}

func (f *finOpsFakeProvider) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	model, _ := renderedContext["model"].(string)
	return providers.NormalizedChatResponse(model, "hi", "stop", 1000, 500), nil
}
func (f *finOpsFakeProvider) CachingCapability() string { return "none" }
func (f *finOpsFakeProvider) CostModel() string         { return f.costModel }

// TestFinOpsDashboardShowsRealCostPerModel is OBS-003's acceptance test: a real /decide call with
// known token usage, routed through a real config-as-code pricing table, produces a real cost that
// is durably recorded (real Postgres) and then correctly aggregated on the real HTML dashboard —
// alongside a compute_based model, whose row must show its own cost_model honestly rather than a
// fabricated per-token price.
func TestFinOpsDashboardShowsRealCostPerModel(t *testing.T) {
	s := newAPITestStore(t)
	ledger := s.FinOpsLedger()

	tokenModel := "finops-test-model-" + randSuffix(t)
	computeModel := "finops-test-local-" + randSuffix(t)

	pricing := finops.NewPricingTable([]finops.Rate{
		{Provider: "fake-token", Model: tokenModel, CostModel: "token_based", InputPerMillionUSD: 10, OutputPerMillionUSD: 30},
		{Provider: "fake-compute", Model: computeModel, CostModel: "compute_based"},
	})

	gw := modelgateway.New()
	gw.RegisterProvider("fake-token", &finOpsFakeProvider{costModel: "token_based"})
	gw.RegisterProvider("fake-compute", &finOpsFakeProvider{costModel: "compute_based"})

	mux := http.NewServeMux()
	(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledger}).Register(mux)
	(&FinOpsHandlers{Ledger: ledger}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// 1000 prompt + 500 completion tokens at $10/$30 per million = 0.01 + 0.015 = $0.025.
	status, body := postDecide(t, srv, decideRequest{
		Candidates:      []decideCandidate{{Provider: "fake-token", Model: tokenModel, Priority: 0}},
		RenderedContext: map[string]any{"messages": []any{}},
	})
	if status != http.StatusOK {
		t.Fatalf("POST /decide (token_based): status=%d body=%v", status, body)
	}
	costUSD, ok := body["cost_usd"].(float64)
	if !ok {
		t.Fatalf("expected a numeric cost_usd in the /decide response, got %v", body["cost_usd"])
	}
	if diff := costUSD - 0.025; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost_usd = %v, want ~0.025", costUSD)
	}
	if body["cost_model"] != "token_based" {
		t.Fatalf("cost_model = %v, want token_based", body["cost_model"])
	}

	status, body = postDecide(t, srv, decideRequest{
		Candidates:      []decideCandidate{{Provider: "fake-compute", Model: computeModel, Priority: 0}},
		RenderedContext: map[string]any{"messages": []any{}},
	})
	if status != http.StatusOK {
		t.Fatalf("POST /decide (compute_based): status=%d body=%v", status, body)
	}
	if _, hasCost := body["cost_usd"]; hasCost {
		t.Fatalf("expected no cost_usd for a compute_based provider (unpriced, not $0), got %v", body["cost_usd"])
	}
	if body["cost_model"] != "compute_based" {
		t.Fatalf("cost_model = %v, want compute_based", body["cost_model"])
	}

	resp, err := http.Get(srv.URL + "/finops/costs")
	if err != nil {
		t.Fatalf("GET /finops/costs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	buf := make([]byte, 64*1024)
	n, _ := resp.Body.Read(buf)
	page := string(buf[:n])

	if !strings.Contains(page, tokenModel) || !strings.Contains(page, "0.0250") {
		t.Errorf("expected the dashboard to show the real token_based cost for %s, got:\n%s", tokenModel, page)
	}
	if !strings.Contains(page, computeModel) || !strings.Contains(page, "compute_based") {
		t.Errorf("expected the dashboard to show the compute_based model with its own cost_model, got:\n%s", page)
	}
}
