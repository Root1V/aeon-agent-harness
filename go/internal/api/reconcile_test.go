package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// platformSource adapts the prometheus_inference client to finops.UsageSource. The adapting happens
// here rather than in finops because that package must not import a concrete provider — MDL-017
// removed the last provider name from the routing core for the same reason.
type platformSource struct{ client *prometheusinference.Client }

func (p platformSource) PlatformUsage(ctx context.Context, requestID string) (*finops.PlatformUsage, error) {
	rec, err := p.client.Usage(ctx, requestID)
	if err != nil {
		return nil, err
	}
	// UsageRecord is flat since MDL-009: the Axonium SDK types every counter as a pointer (nil and
	// zero mean different things there), and the mapping to Aeon's vocabulary happens in one place
	// rather than at every call site.
	return &finops.PlatformUsage{
		RequestID:            rec.RequestID,
		Model:                rec.Model,
		PromptTokens:         rec.PromptTokens,
		CompletionTokens:     rec.CompletionTokens,
		CostUSD:              rec.CostUSD,
		PromptPricePer1M:     rec.PromptPricePer1M,
		CompletionPricePer1M: rec.CompletionPricePer1M,
	}, nil
}

// TestLedgerTotalsReconcileWithPlatformUsage is OBS-007's acceptance test.
//
// Aeon and Prometheus kept two independent accountings of the same calls and nothing compared them.
// They measure different things on purpose — the platform per request, us per profile and per run,
// which is what a budget holder looks at — but a systematic difference between them was the one error
// neither could detect alone.
//
// Two things had to exist before any of this was possible, and neither did:
//
//   - A join key. The platform's id for a call arrives in the `x-request-id` response HEADER. The body
//     carries `id: chatcmpl-...` and the headers also carry `x-trace-id`, and GET /v1/usage/{id}
//     answers 404 for both of those — measured. Nothing recorded the right one, so the two ledgers had
//     no column in common.
//   - A cost on their side we could read. It is there, needs no admin scope, and comes with the RATES
//     APPLIED, which is what makes this a comparison of two derivations rather than a copy of one
//     number.
//
// This runs against the real platform. A fake would assert the shape of a response we wrote, and the
// property under test is precisely whether our figures match someone else's.
func TestLedgerTotalsReconcileWithPlatformUsage(t *testing.T) {
	ctx := context.Background()

	gateway := os.Getenv("PROMETHEUS_GATEWAY_URL")
	authURL := os.Getenv("PROMETHEUS_AUTH_URL")
	clientID := os.Getenv("PROMETHEUS_CLIENT_ID")
	secret := os.Getenv("PROMETHEUS_CLIENT_SECRET")
	model := os.Getenv("AEON_TEST_RECONCILE_MODEL")
	if model == "" {
		model = "qwen3-0.6b"
	}
	if gateway == "" || authURL == "" || clientID == "" || secret == "" {
		t.Skip("Prometheus credentials not set — OBS-007 reconciles against the real platform or not at all")
	}

	client := &prometheusinference.Client{GatewayURL: gateway, ClientID: clientID, ClientSecret: secret, Scope: "inference:read model:" + model}
	source := platformSource{client: client}

	t.Run("a real call lands in our ledger with the platform's own id", func(t *testing.T) {
		s := newAPITestStore(t)
		ledgerStore := s
		ledger := s.FinOpsLedgerFor("default")

		// The rate here is deliberately the platform's real one for this model, discovered by asking
		// it rather than assumed: our bundle declared these models compute_based with no per-token
		// price, which is what made every local call unpriced. See the divergence subtest below.
		pricing := finops.NewPricingTable([]finops.Rate{{
			Provider: prometheusinference.Name, Model: model, CostModel: "token_based",
			InputPerMillionUSD: 0.2, OutputPerMillionUSD: 0.6,
		}})

		gw := modelgateway.New()
		gw.RegisterProvider(prometheusinference.Name, &prometheusinference.Adapter{Client: client, Model: model})
		handlers := &ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledgerStore}

		mux := http.NewServeMux()
		handlers.Register(mux)
		srv := httptest.NewServer(authWrap(t, mux))
		t.Cleanup(srv.Close)

		status, body := postDecide(t, srv, decideRequest{
			Candidates: []decideCandidate{{Provider: prometheusinference.Name, Model: model, Priority: 0}},
			RenderedContext: map[string]any{
				"messages":   []any{map[string]any{"role": "user", "content": "Responde solo: ok"}},
				"max_tokens": 24,
			},
		})
		if status != 200 {
			t.Fatalf("POST /decide against the real platform: status=%d body=%v", status, body)
		}
		output, _ := body["output"].(map[string]any)
		requestID, _ := output["provider_request_id"].(string)
		if requestID == "" {
			t.Fatal("the normalized response carries no provider_request_id — without it the two ledgers have no key in common and nothing can be reconciled")
		}
		t.Logf("platform request id: %s", requestID)

		rows, err := ledger.RowsWithProviderRequestID(ctx, prometheusinference.Name, 10)
		if err != nil {
			t.Fatalf("RowsWithProviderRequestID: %v", err)
		}
		var found *store.LedgerRow
		for i := range rows {
			if rows[i].ProviderRequestID == requestID {
				found = &rows[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("no ledger row carries request id %s — the id reached the response but not the table", requestID)
		}

		// Now the reconciliation itself, over a call that really happened.
		report, err := finops.Reconcile(ctx, source, []finops.LedgerRow{{
			Provider: found.Provider, Model: found.Model,
			PromptTokens: found.PromptTokens, CompletionTokens: found.CompletionTokens,
			CostUSD: found.CostUSD, ProviderRequestID: found.ProviderRequestID,
		}})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if report.Compared != 1 {
			t.Fatalf("Compared = %d, want 1", report.Compared)
		}
		for _, d := range report.Divergences {
			t.Errorf("divergence [%s]: %s", d.Kind, d.Detail)
		}
		if report.Agreed != 1 {
			t.Errorf("Agreed = %d, want 1 — our figures do not match the platform's for a call we just made", report.Agreed)
		}
	})

	t.Run("the platform's cost is checked against its own rates, not just copied", func(t *testing.T) {
		// This is what "reconcile" has to mean to be worth building. Asking the platform for a number
		// and storing it would agree with itself by construction. Recomputing it from the rates it
		// published for that same request catches a fault on their side without asking them.
		usage, err := source.PlatformUsage(ctx, freshRequestID(t, client, model))
		if err != nil {
			t.Fatalf("PlatformUsage: %v", err)
		}
		if usage.CostUSD == nil {
			t.Skip("this call came back unpriced — a real state on this platform, but not the one under test here")
		}
		if usage.PromptPricePer1M == nil || usage.CompletionPricePer1M == nil {
			// KNOWN GAP, named rather than hidden (MDL-009). The platform DOES return the applied rates
			// — measured directly over HTTP on 2026-09-26, `rates: {prompt_price_per_1m: 0.2,
			// completion_price_per_1m: 0.6}` — but the Axonium SDK neither models them nor populates
			// RequestUsage.Raw, which its own doc says exists "so backend-specific fields this SDK does
			// not model stay reachable rather than being dropped". Measured: Raw == nil on every usage
			// row, so they are dropped.
			//
			// A skip and not a weakened assertion: the moment the SDK populates Raw or types Rates, this
			// becomes a real check again by itself. Weakening it would mean losing the property quietly,
			// which is the shape this whole phase has been removing. Reported to Axonium.
			//
			// What is NOT lost meanwhile: OBS-007's core. Comparing OUR figure against THEIRS still works
			// on cost_usd alone. What sleeps is the check on the PLATFORM's own self-consistency —
			// recomputing their cost from their published rates — which is a check on their arithmetic,
			// not ours.
			t.Skip("the Axonium SDK does not surface the applied rates (RequestUsage.Raw is nil) — see the comment above; reported to them")
		}
		recomputed := (float64(usage.PromptTokens)**usage.PromptPricePer1M +
			float64(usage.CompletionTokens)**usage.CompletionPricePer1M) / 1e6
		t.Logf("platform cost %g, recomputed from its own rates %g", *usage.CostUSD, recomputed)

		report, err := finops.Reconcile(ctx, source, []finops.LedgerRow{{
			Provider: prometheusinference.Name, Model: usage.Model,
			PromptTokens: tokens(usage.PromptTokens), CompletionTokens: tokens(usage.CompletionTokens),
			CostUSD: usage.CostUSD, ProviderRequestID: usage.RequestID,
		}})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		for _, d := range report.Divergences {
			if d.Kind == "self_inconsistent" {
				t.Errorf("the platform's own cost disagrees with its own rates: %s", d.Detail)
			}
		}
	})

	t.Run("an unpriced row on our side is reported, not counted as agreement", func(t *testing.T) {
		// The defect that opened this feature, as a property. Our bundle declares these models
		// compute_based, so the ledger prices nothing while the platform prices every call. Before
		// OBS-007 the two numbers simply never met, so this produced no signal at all.
		requestID := freshRequestID(t, client, model)
		report, err := finops.Reconcile(ctx, source, []finops.LedgerRow{{
			Provider: prometheusinference.Name, Model: model,
			PromptTokens: tokens(0), CompletionTokens: tokens(0), // measured zero: what a compute_based row looked like
			CostUSD:           nil, // what a compute_based row looks like in our ledger
			ProviderRequestID: requestID,
		}})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if report.Agreed != 0 {
			t.Error("a row we could not price was counted as agreeing with a platform that priced it")
		}
		var kinds []string
		for _, d := range report.Divergences {
			kinds = append(kinds, d.Kind)
		}
		if !contains(kinds, "unpriced_here") {
			t.Errorf("divergences = %v, want unpriced_here reported", kinds)
		}
		if !contains(kinds, "tokens") {
			t.Errorf("divergences = %v, want the token mismatch reported too", kinds)
		}
	})

	t.Run("a row with no join key is unreconciled, never agreed", func(t *testing.T) {
		// Counting an uncomparable row as agreement is how a reconciliation reports success over data
		// it never looked at — the same shape as a skipped test counting as green.
		report, err := finops.Reconcile(ctx, source, []finops.LedgerRow{{
			Provider: prometheusinference.Name, Model: model, ProviderRequestID: "",
		}})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if report.Agreed != 0 || report.Compared != 0 {
			t.Errorf("Agreed=%d Compared=%d, want 0 and 0", report.Agreed, report.Compared)
		}
		if report.Unreconciled != 1 {
			t.Errorf("Unreconciled = %d, want 1", report.Unreconciled)
		}
	})
}

// freshRequestID makes one real call and returns the platform's id for it.
func freshRequestID(t *testing.T, client *prometheusinference.Client, model string) string {
	t.Helper()
	_, meta, err := client.ChatCompletionWithMeta(context.Background(), map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ok"}},
		"max_tokens": 16,
	})
	if err != nil {
		t.Fatalf("chat completion against the real platform: %v", err)
	}
	if meta.RequestID == "" {
		t.Fatal("the platform returned no x-request-id")
	}
	return meta.RequestID
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
