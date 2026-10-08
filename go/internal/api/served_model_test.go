package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/providers"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// disagreeingProvider answers with a different model than it was asked for, and names the deployment.
//
// A controlled provider and not the real platform, on purpose and stated: the case that motivates
// OBS-005 is AXONIUM'S measurement — asking for an instance-specific name and being served another —
// and we have no scope for those names, so it is not one I reproduced. Claiming otherwise would be
// asserting someone else's finding as my own. What the real platform DOES verify is the recording path,
// in the subtest below.
type disagreeingProvider struct {
	servedModel string
	instance    string
}

func (p *disagreeingProvider) Decide(_ context.Context, _ map[string]any) (map[string]any, error) {
	out := map[string]any{
		"model": p.servedModel,
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "ok"},
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	}
	if p.instance != "" {
		out["served_by_instance"] = p.instance
	}
	return out, nil
}

func (p *disagreeingProvider) CachingCapability() string { return "none" }
func (p *disagreeingProvider) CostModel() string         { return "token_based" }

// TestServedModelIsRecordedWhenItDiffers is OBS-005's acceptance test.
//
// The ledger imputes cost by the ModelPolicyBundle's model, which is correct and does NOT change here:
// you pay for the profile, not for the deployment. But the normalized response carries the provider's
// own answer to "which model was this", and it can be a different one. Two answers to one question
// lived in the same call, nothing reconciled them, and — worse — nothing recorded either the second
// answer or WHICH DEPLOYMENT served it, which is the question an incident starts from.
//
// Measured against the live deployment while building this: 6 of 6 calls to qwen3-0.6b were served by
// the same instance and response.model equalled what was asked. That matters beyond reassurance,
// because the roadmap made it conditional: since the serving deployment does NOT vary between calls of
// one profile, this stays a recording problem. If it ever varies, it becomes a routing problem and
// MDL-013 ages with it.
func TestServedModelIsRecordedWhenItDiffers(t *testing.T) {
	ctx := context.Background()

	newServer := func(t *testing.T, ledgerStore *store.Store, provider providers.Provider, model string) *httptest.Server {
		t.Helper()
		gw := modelgateway.New()
		gw.RegisterProvider("test-provider", provider)
		pricing := finops.NewPricingTable([]finops.Rate{{
			Provider: "test-provider", Model: model, CostModel: "token_based",
			InputPerMillionUSD: 1, OutputPerMillionUSD: 2,
		}})
		mux := http.NewServeMux()
		(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledgerStore}).Register(mux)
		srv := httptest.NewServer(authWrap(t, mux))
		t.Cleanup(srv.Close)
		return srv
	}

	decide := func(t *testing.T, srv *httptest.Server, model string) map[string]any {
		t.Helper()
		status, body := postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: "test-provider", Model: model, Priority: 0}},
			RenderedContext: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		})
		if status != http.StatusOK {
			t.Fatalf("POST /decide: status=%d body=%v", status, body)
		}
		return body
	}

	t.Run("a served model that differs is reported, not left to be deduced", func(t *testing.T) {
		ledgerStore := newAPITestStore(t)
		ledger := ledgerStore.FinOpsLedgerFor("default")
		requested := "profile-model-" + randSuffix(t)
		srv := newServer(t, ledgerStore, &disagreeingProvider{servedModel: "actually-served-2", instance: "inst-7"}, requested)

		body := decide(t, srv, requested)

		if body["served_model"] != "actually-served-2" {
			t.Errorf("served_model = %v, want actually-served-2", body["served_model"])
		}
		if body["served_model_differs"] != true {
			t.Error("served_model_differs is not set — a caller would have to compare two fields to notice, which is exactly what 'deducible' means")
		}
		if body["served_by_instance"] != "inst-7" {
			t.Errorf("served_by_instance = %v, want inst-7", body["served_by_instance"])
		}

		// The imputation must NOT have moved. This is the assertion that keeps the feature honest:
		// recording what served is worthless if it quietly changes who gets billed.
		if body["model"] != requested {
			t.Errorf("model = %v, want the bundle's %q — cost is imputed to the profile, not the deployment", body["model"], requested)
		}

		row := onlyRowFor(t, ledger, "test-provider", requested)
		if row.ServedModel != "actually-served-2" {
			t.Errorf("ledger served_model = %q, want actually-served-2", row.ServedModel)
		}
		if row.ServedByInstance != "inst-7" {
			t.Errorf("ledger served_by_instance = %q, want inst-7 — the deployment that answered was recorded nowhere before this", row.ServedByInstance)
		}
		if row.Model != requested {
			t.Errorf("ledger model = %q, want the bundle's %q", row.Model, requested)
		}
	})

	t.Run("agreement is silent, and absence is not an echo", func(t *testing.T) {
		// Two things at once, and the second is the easy one to get wrong. When the provider serves what
		// was asked, nothing is reported — a discrepancy flag that fires on every call is noise. And
		// when a provider says nothing about the instance, the column stays empty rather than being
		// filled with the requested model: "served what we asked" and "never said" are different facts,
		// and only one of them can be reconciled later.
		ledgerStore := newAPITestStore(t)
		ledger := ledgerStore.FinOpsLedgerFor("default")
		requested := "agreeing-model-" + randSuffix(t)
		srv := newServer(t, ledgerStore, &disagreeingProvider{servedModel: requested}, requested)

		body := decide(t, srv, requested)

		if _, present := body["served_model"]; present {
			t.Errorf("served_model is reported for a provider that served what was asked: %v", body["served_model"])
		}
		if _, present := body["served_model_differs"]; present {
			t.Error("served_model_differs is set on an agreeing call")
		}

		row := onlyRowFor(t, ledger, "test-provider", requested)
		if row.ServedByInstance != "" {
			t.Errorf("served_by_instance = %q, want empty — this provider reported no instance, and echoing something would invent a fact", row.ServedByInstance)
		}
	})

	t.Run("the real platform's own deployment is recorded", func(t *testing.T) {
		gateway := os.Getenv("PROMETHEUS_GATEWAY_URL")
		clientID := os.Getenv("PROMETHEUS_CLIENT_ID")
		secret := os.Getenv("PROMETHEUS_CLIENT_SECRET")
		model := os.Getenv("AEON_TEST_RECONCILE_MODEL")
		if model == "" {
			model = "qwen3-0.6b"
		}
		if gateway == "" || clientID == "" || secret == "" {
			t.Skip("Prometheus credentials not set — the recording path is verified against the real platform or not at all")
		}

		ledgerStore := newAPITestStore(t)
		ledger := ledgerStore.FinOpsLedgerFor("default")
		client := &prometheusinference.Client{
			GatewayURL: gateway, ClientID: clientID, ClientSecret: secret,
			Scope: "inference:read model:" + model,
		}
		gw := modelgateway.New()
		gw.RegisterProvider(prometheusinference.Name, &prometheusinference.Adapter{Client: client, Model: model})
		pricing := finops.NewPricingTable([]finops.Rate{{
			Provider: prometheusinference.Name, Model: model, CostModel: "token_based",
			InputPerMillionUSD: 0.2, OutputPerMillionUSD: 0.6,
		}})
		mux := http.NewServeMux()
		(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledgerStore}).Register(mux)
		srv := httptest.NewServer(authWrap(t, mux))
		t.Cleanup(srv.Close)

		status, body := postDecide(t, srv, decideRequest{
			Candidates: []decideCandidate{{Provider: prometheusinference.Name, Model: model, Priority: 0}},
			RenderedContext: map[string]any{
				"messages":   []any{map[string]any{"role": "user", "content": "Responde solo: ok"}},
				"max_tokens": 16,
			},
		})
		if status != http.StatusOK {
			t.Fatalf("POST /decide against the real platform: status=%d body=%v", status, body)
		}
		instance, _ := body["served_by_instance"].(string)
		if instance == "" {
			t.Fatal("the real platform served this call and no instance was recorded — that id is what the platform team asks for about a slow or odd response")
		}

		rows, err := ledger.RowsForModel(ctx, prometheusinference.Name, model, 5)
		if err != nil {
			t.Fatalf("RowsForModel: %v", err)
		}
		if len(rows) == 0 || rows[0].ServedByInstance == "" {
			t.Fatalf("no ledger row carries the serving instance: %+v", rows)
		}
		t.Logf("real call served by %s, response.model agreed with the request (%s)", rows[0].ServedByInstance, model)

		// Today there is no discrepancy on this deployment — measured 6/6 — so asserting one would be
		// asserting a bug we do not have. What is asserted is that agreement is reported AS agreement.
		if _, differs := body["served_model_differs"]; differs {
			t.Errorf("the platform served a different model than requested: served=%v requested=%v", body["served_model"], model)
		}
	})
}

// onlyRowFor reads back the single row recorded for a (provider, model) pair. Each subtest uses a fresh
// randomised model name, so exactly one row is the correct expectation and more than one would mean the
// handler wrote twice.
func onlyRowFor(t *testing.T, ledger *store.FinOpsLedger, provider, model string) store.LedgerRow {
	t.Helper()
	rows, err := ledger.RowsForModel(context.Background(), provider, model, 5)
	if err != nil {
		t.Fatalf("RowsForModel: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows for %s/%s, want exactly 1: %+v", len(rows), provider, model, rows)
	}
	return rows[0]
}
