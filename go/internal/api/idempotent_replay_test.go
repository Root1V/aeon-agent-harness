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

// TestIdempotentReplayIsNotBilledTwice is OBS-006's acceptance test.
//
// The platform serves an identical request under the same Idempotency-Key from a stored response, and
// the usage block it returns REPEATS the original generation's tokens. recordCost read that block and
// added a second row with the same tokens and the same price, so a resumed run inflated its own cost
// — upward, which is the direction nobody audits because it looks prudent. Same family as MDL-014's
// fabricated zero and MDL-012's invisible cache: nothing fails, the books are wrong.
//
// Measured against the live deployment on 2026-09-26, and the shape is why this needs a real platform
// rather than a fixture:
//
//	call 1:  x-request-id: 609cac87…                usage 12/20, cost 1.44e-05
//	call 2:  idempotent-replay: true
//	         x-idempotent-replay-of: 609cac87…      the generation that was billed
//	         x-request-id: 86997f3a…                its own, new id
//	         usage 12/20                            THE SAME tokens, already paid for
//
// And the replay's own request id returns 404 from /v1/usage — there is no accounting for it, because
// the accounting belongs to the generation. That is the second half of this feature: OBS-007's
// reconciliation must not report a replay as a missing row, or every replay becomes a false
// divergence and buries the real ones.
func TestIdempotentReplayIsNotBilledTwice(t *testing.T) {
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
		t.Skip("Prometheus credentials not set — OBS-006 needs the platform's own replay headers, which no fixture can produce honestly")
	}

	client := &prometheusinference.Client{
		GatewayURL: gateway,
		Tokens: &prometheusinference.TokenSource{
			AuthURL: authURL, ClientID: clientID, ClientSecret: secret,
			Scope: "inference:read model:" + model,
		},
	}

	t.Run("the platform marks a replay, and it repeats the original usage", func(t *testing.T) {
		// Establishes the premise from the platform itself rather than from a fixture. If this ever
		// stops holding, everything below is testing a world that no longer exists.
		key := "aeon-obs006-" + randSuffix(t)
		first, firstID, firstReplay, err := chatWithKey(ctx, client, model, key)
		if err != nil {
			t.Fatalf("first call: %v", err)
		}
		if firstReplay != "" {
			t.Fatalf("the FIRST call came back marked as a replay of %s", firstReplay)
		}

		second, secondID, secondReplay, err := chatWithKey(ctx, client, model, key)
		if err != nil {
			t.Fatalf("second call: %v", err)
		}
		if secondReplay == "" {
			t.Fatal("the second identical call under the same key was not marked as a replay — this platform no longer replays, and OBS-006's premise is gone")
		}
		if secondReplay != firstID {
			t.Errorf("x-idempotent-replay-of = %q, want the first call's id %q", secondReplay, firstID)
		}
		if secondID == firstID {
			t.Error("the replay reused the original request id; it is supposed to carry its own")
		}

		fp, fc := usageOf(first)
		sp, sc := usageOf(second)
		if fp != sp || fc != sc {
			t.Errorf("replay usage %d/%d differs from the original %d/%d — the premise of this feature is that it REPEATS them", sp, sc, fp, fc)
		}
		if sp == 0 && sc == 0 {
			t.Error("the replay reported zero tokens, so nothing would have been double-billed and this test proves nothing")
		}
		t.Logf("original %s usage %d/%d — replay %s repeats %d/%d", firstID, fp, fc, secondID, sp, sc)
	})

	t.Run("a replay is recorded at zero cost, naming the generation that was billed", func(t *testing.T) {
		ledger := newAPITestStore(t).FinOpsLedger()
		pricing := finops.NewPricingTable([]finops.Rate{{
			Provider: prometheusinference.Name, Model: model, CostModel: "token_based",
			InputPerMillionUSD: 0.2, OutputPerMillionUSD: 0.6,
		}})
		srv := realPrometheusDecideServer(t, client, model, pricing, ledger)

		key := "aeon-obs006-ledger-" + randSuffix(t)
		firstBody := decideWithKey(t, srv, model, key)
		if _, replayed := firstBody["idempotent_replay_of"]; replayed {
			t.Fatal("the first call was already a replay — pick a fresh key")
		}
		firstCost, ok := firstBody["cost_usd"].(float64)
		if !ok || firstCost <= 0 {
			t.Fatalf("the first call recorded cost_usd=%v, want a real positive cost", firstBody["cost_usd"])
		}

		secondBody := decideWithKey(t, srv, model, key)
		replayOf, _ := secondBody["idempotent_replay_of"].(string)
		if replayOf == "" {
			t.Fatal("the gateway did not surface idempotent_replay_of, so nothing downstream can tell a replay from a generation")
		}
		if got := secondBody["cost_usd"]; got != float64(0) {
			t.Errorf("the replay was billed %v — the platform charged nothing for it", got)
		}

		rows, err := ledger.RowsWithProviderRequestID(ctx, prometheusinference.Name, 20)
		if err != nil {
			t.Fatalf("RowsWithProviderRequestID: %v", err)
		}
		var generation, replay *store.LedgerRow
		for i := range rows {
			switch {
			case rows[i].IdempotentReplayOf != "":
				if replay == nil {
					replay = &rows[i]
				}
			case generation == nil:
				generation = &rows[i]
			}
		}
		if replay == nil {
			t.Fatal("no replay row in the ledger — a call that happened left no trace, which an audit cannot tell from a call that never happened (OBS-008)")
		}
		if replay.CostUSD == nil || *replay.CostUSD != 0 {
			t.Errorf("replay row cost = %v, want a measured 0: no new money was spent, and NULL would read as unpriced", replay.CostUSD)
		}
		if replay.PromptTokens != 0 || replay.CompletionTokens != 0 {
			t.Errorf("replay row recorded %d/%d tokens — they belong to the generation, and counting them here inflates the totals the same way the cost would",
				replay.PromptTokens, replay.CompletionTokens)
		}
		if replay.IdempotentReplayOf == "" {
			t.Error("the replay row does not name the generation that was billed")
		}
		if generation == nil || generation.CostUSD == nil || *generation.CostUSD <= 0 {
			t.Error("the generation's own row is missing or unpriced — the replay must not be the only row")
		}
	})

	t.Run("reconciliation treats a replay as agreed, not as missing", func(t *testing.T) {
		// A replay has no usage row on the platform by design. Fetching it would report a `missing`
		// divergence for every replay and bury the real ones.
		report, err := finops.Reconcile(ctx, platformSource{client: client}, []finops.LedgerRow{{
			Provider: prometheusinference.Name, Model: model,
			CostUSD:            float64Ptr(0),
			ProviderRequestID:  "a-replay-id-with-no-usage-row",
			IdempotentReplayOf: "the-generation-that-was-billed",
		}})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if report.Agreed != 1 || len(report.Divergences) != 0 {
			t.Fatalf("Agreed=%d divergences=%+v, want a replay at zero cost to agree", report.Agreed, report.Divergences)
		}
	})

	t.Run("a replay that WAS billed is reported, which is the defect itself", func(t *testing.T) {
		// The reconciliation now detects the bug OBS-006 fixed, so a regression surfaces as a
		// divergence rather than as a quietly larger number.
		report, err := finops.Reconcile(ctx, platformSource{client: client}, []finops.LedgerRow{{
			Provider: prometheusinference.Name, Model: model,
			CostUSD:            float64Ptr(1.44e-05),
			ProviderRequestID:  "a-replay-id",
			IdempotentReplayOf: "the-generation-that-was-billed",
		}})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if report.Agreed != 0 {
			t.Error("a replay carrying a real cost was counted as agreeing")
		}
		found := false
		for _, d := range report.Divergences {
			if d.Kind == "replay_billed" {
				found = true
			}
		}
		if !found {
			t.Errorf("divergences = %+v, want replay_billed", report.Divergences)
		}
	})
}

func float64Ptr(v float64) *float64 { return &v }

// usageOf pulls the prompt/completion counts out of a raw platform response.
func usageOf(body map[string]any) (prompt, completion int) {
	usage, _ := body["usage"].(map[string]any)
	return toInt(usage["prompt_tokens"]), toInt(usage["completion_tokens"])
}

// chatWithKey makes one real call under an idempotency key, returning the body, the platform's id for
// this call, and the id of the generation it replays (empty when it really generated).
func chatWithKey(ctx context.Context, client *prometheusinference.Client, model, key string) (map[string]any, string, string, error) {
	return client.ChatCompletionWithMeta(ctx, map[string]any{
		"model":                                 model,
		"messages":                              []any{map[string]any{"role": "user", "content": "Responde solo: ok"}},
		"max_tokens":                            20,
		prometheusinference.IdempotencyKeyField: key,
	})
}

// realPrometheusDecideServer wires the real handler against the real adapter, so the path under test
// is recordCost's own on a real replay.
func realPrometheusDecideServer(t *testing.T, client *prometheusinference.Client, model string, pricing *finops.PricingTable, ledger *store.FinOpsLedger) *httptest.Server {
	t.Helper()
	gw := modelgateway.New()
	gw.RegisterProvider(prometheusinference.Name, &prometheusinference.Adapter{Client: client, Model: model})
	mux := http.NewServeMux()
	(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledger}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// decideWithKey posts /decide with an idempotency key in the rendered context.
func decideWithKey(t *testing.T, srv *httptest.Server, model, key string) map[string]any {
	t.Helper()
	status, body := postDecide(t, srv, decideRequest{
		Candidates: []decideCandidate{{Provider: prometheusinference.Name, Model: model, Priority: 0}},
		RenderedContext: map[string]any{
			"messages":                              []any{map[string]any{"role": "user", "content": "Responde solo: ok"}},
			"max_tokens":                            20,
			prometheusinference.IdempotencyKeyField: key,
		},
	})
	if status != http.StatusOK {
		t.Fatalf("POST /decide: status=%d body=%v", status, body)
	}
	return body
}
