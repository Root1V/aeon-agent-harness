package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// VRT-AEON-003 A-3's acceptance test: /v1/chat/completions is governed like /decide.
//
// THE DEFECT, which is a shape worth naming rather than a missing feature. The endpoint's own doc
// comment said "budget/cost enforcement at this endpoint is not implemented yet" — an honest note,
// and also the whole problem: the same deployment had a governed door and an ungoverned one, and the
// ungoverned one was the one documented as needing no code change to adopt. A platform whose budget
// can be bypassed by changing a base_url does not have a budget; it has a convention.
//
// Real Postgres, because the subject IS the ledger and the registry — the ceiling is read from a
// manifest in the agent registry and checked against rows in the cost ledger, and a fake for either
// would be a test of the fake.
//
// A controlled provider rather than real inference, and this is a deliberate split from A-2's tests.
// A-2 is about FIDELITY, where a double proves nothing because the old code's requests were ACCEPTED
// by the platform. A-3 is about OUR accounting: a ceiling at exactly N tokens needs the provider to
// report exactly N, and real inference reports whatever it reports. A ceiling test against real
// inference is a flaky test, not a stronger one. The real-platform half that does need the platform —
// idempotency, whose replay headers no fixture can produce honestly — is in
// TestIdempotencyHeaderIsNotBilledTwiceThroughTheOpenAISurface below.
func TestOpenAICompatibleSurfaceIsGovernedLikeDecide(t *testing.T) {
	ctx := context.Background()

	t.Run("a run with a token ceiling is stopped, and the stop is recorded", func(t *testing.T) {
		s := newAPITestStore(t)
		ledger := s.FinOpsLedgerFor("default")
		agents := s.AgentRegistryFor("default")

		// 1500 tokens per call (finOpsFakeProvider reports 1000 + 500), ceiling 3000: two calls pass,
		// the third is refused. Same arithmetic as MDL-017's /decide test, deliberately — if the two
		// surfaces behave differently on identical numbers, that difference is the bug.
		model := "openai-gov-" + randSuffix(t)
		agentName := "openai-gov-agent-" + randSuffix(t)
		agentRef := agentName + "@0.1.0"
		if _, err := agents.Create(ctx, map[string]any{
			"apiVersion": "harness.ai/v1", "kind": "Agent",
			"metadata": map[string]any{"name": agentName, "version": "0.1.0", "owner": "test", "lifecycle": "Draft"},
			"spec":     map[string]any{"runtime": map[string]any{"budgets": map[string]any{"tokens": 3000}}},
		}, "test"); err != nil {
			t.Fatalf("registering the agent: %v", err)
		}

		srv := newGovernedOpenAIServer(t, s, model, "compute_based")
		runID := "openai-gov-run-" + randSuffix(t)

		call := func() (int, map[string]any) {
			return postChatCompletionWithHeaders(t, srv, map[string]any{
				"model":    "governed-test",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}, map[string]string{
				RunIDHeader:            runID,
				AgentManifestRefHeader: agentRef,
			})
		}

		for i := 1; i <= 2; i++ {
			status, body := call()
			if status != http.StatusOK {
				t.Fatalf("call %d was refused before the ceiling: status=%d body=%v", i, status, body)
			}
			// Each allowed call must report that it WAS governed. "It worked" is the same observable
			// outcome as the old ungoverned endpoint; the report is the difference.
			aeon := aeonBlock(t, body)
			if governed, _ := aeon["governed"].(bool); !governed {
				t.Fatalf("call %d reports governed=false with both headers present and a ledger "+
					"configured: %v", i, aeon)
			}
			if aeon["run_id"] != runID {
				t.Errorf("call %d does not echo the run it was attributed to: %v", i, aeon)
			}
		}

		status, body := call()
		if status != http.StatusPaymentRequired {
			t.Fatalf("the third call returned %d, want 402 at 3000 tokens: %v", status, body)
		}
		// OpenAI's error envelope, so a client that understands only that envelope still reads a
		// message rather than an unparsed object.
		errObj, ok := body["error"].(map[string]any)
		if !ok {
			t.Fatalf("the refusal is not in OpenAI's error shape: %v", body)
		}
		if errType, _ := errObj["type"].(string); errType != "aeon_budget_exceeded" {
			t.Errorf("error.type = %q, want aeon_budget_exceeded — a client cannot tell a budget stop "+
				"from a routing failure otherwise", errType)
		}
		if msg, _ := errObj["message"].(string); !strings.Contains(msg, "budget exceeded") {
			t.Errorf("error.message = %q, want it to say what happened", msg)
		}
		// And the facts beside it: a caller has to be able to act on the refusal, which means knowing
		// the ceiling, the spend, and that retrying is pointless.
		aeon := aeonBlock(t, body)
		if retryable, present := aeon["retryable"].(bool); !present || retryable {
			t.Errorf("the refusal does not say retryable=false; a worker mapping this to a retry would "+
				"loop on a call that can never be allowed again: %v", aeon)
		}
		if tokens, _ := aeon["tokens"].(float64); tokens < 3000 {
			t.Errorf("the refusal reports %v tokens, want at least the ceiling: %v", tokens, aeon)
		}

		// THE RECORDED PART. 3000 and not 4500: the refused call never reached the provider, so it
		// left no row. A higher number here would mean the ceiling is a receipt rather than a cap.
		spend, err := ledger.SpendForRun(ctx, runID)
		if err != nil {
			t.Fatalf("SpendForRun: %v", err)
		}
		if spend.Tokens != 3000 {
			t.Fatalf("ledger tokens = %d, want 3000 (two allowed calls at 1500) — this is the assertion "+
				"that the cost was recorded PER RUN through this endpoint at all", spend.Tokens)
		}
		if spend.ModelCalls != 2 {
			t.Errorf("ledger model_calls = %d, want 2", spend.ModelCalls)
		}
	})

	t.Run("a call with no governance headers works, and says it was not governed", func(t *testing.T) {
		// The compatibility requirement, and the reason it needs its own test: the easy way to govern
		// an endpoint is to make the headers mandatory, which would break every client this endpoint
		// exists for. The call must still succeed — and must not succeed SILENTLY, because a caller
		// that assumes its budget applies here is exactly who got hurt by the original defect.
		s := newAPITestStore(t)
		srv := newGovernedOpenAIServer(t, s, "openai-ungoverned-"+randSuffix(t), "compute_based")

		status, body := postChatCompletionWithHeaders(t, srv, map[string]any{
			"model":    "governed-test",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
		if status != http.StatusOK {
			t.Fatalf("a call with no Aeon headers was refused: status=%d body=%v", status, body)
		}
		// Still a valid OpenAI ChatCompletion: the governance block is additive.
		if body["object"] != "chat.completion" {
			t.Errorf("object = %v, want chat.completion", body["object"])
		}
		if _, ok := body["choices"].([]any); !ok {
			t.Errorf("no choices in the response: %v", body)
		}

		aeon := aeonBlock(t, body)
		if governed, _ := aeon["governed"].(bool); governed {
			t.Fatalf("governed=true for a call that named no run and no agent: %v", aeon)
		}
		reasons, _ := aeon["ungoverned_reasons"].([]any)
		if len(reasons) != 2 {
			t.Fatalf("want two reasons (no run id, no agent ref), got %v", aeon["ungoverned_reasons"])
		}
		joined := strings.Join([]string{reasonAt(reasons, 0), reasonAt(reasons, 1)}, " | ")
		for _, header := range []string{RunIDHeader, AgentManifestRefHeader} {
			if !strings.Contains(joined, header) {
				t.Errorf("the reasons do not name %s, so a caller cannot tell what to send: %s", header, joined)
			}
		}
		if idempotent, _ := aeon["idempotent"].(bool); idempotent {
			t.Error("idempotent=true with no Idempotency-Key sent")
		}
	})

	t.Run("a deployment with no ledger reports that, not an absent header", func(t *testing.T) {
		// THE THIRD STATE, and the one a boolean would have merged away: this call sent both headers
		// and is still not governed, because the gateway has no ledger to enforce against. Reporting
		// it as "no run id" would send a caller to fix a header that is already correct.
		gw := modelgateway.New()
		gw.RegisterProvider("fake-compute", &finOpsFakeProvider{costModel: "compute_based"})
		mux := http.NewServeMux()
		(&OpenAICompatibleHandlers{
			Gateway: gw, Bundle: governedTestBundle("ledgerless-" + randSuffix(t)),
			// A gateway deployed without AEON_PG_DSN: governance present, stores absent.
			Governance: &ModelGatewayHandlers{Gateway: gw, Pricing: finops.NewPricingTable(nil)},
		}).Register(mux)
		srv := httptest.NewServer(authWrap(t, mux))
		t.Cleanup(srv.Close)

		status, body := postChatCompletionWithHeaders(t, srv, map[string]any{
			"model":    "governed-test",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, map[string]string{
			RunIDHeader:            "run-" + randSuffix(t),
			AgentManifestRefHeader: "agent@0.1.0",
		})
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%v", status, body)
		}
		aeon := aeonBlock(t, body)
		if governed, _ := aeon["governed"].(bool); governed {
			t.Fatalf("governed=true on a gateway with no ledger: %v", aeon)
		}
		reasons, _ := aeon["ungoverned_reasons"].([]any)
		if len(reasons) != 1 || !strings.Contains(reasonAt(reasons, 0), "no cost ledger") {
			t.Fatalf("want one reason naming the missing ledger, got %v", aeon["ungoverned_reasons"])
		}
	})
}

// newGovernedOpenAIServer mounts the OpenAI-compatible endpoint sharing ONE ModelGatewayHandlers
// instance with /decide — the same thing cmd/aeon-modelgw does, and the reason the two surfaces
// cannot drift apart.
func newGovernedOpenAIServer(t *testing.T, s *store.Store, model, costModel string) *httptest.Server {
	t.Helper()
	gw := modelgateway.New()
	gw.RegisterProvider("fake-compute", &finOpsFakeProvider{costModel: costModel})
	pricing := finops.NewPricingTable([]finops.Rate{
		{Provider: "fake-compute", Model: model, CostModel: costModel},
	})
	governance := &ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: s, Agents: s}
	mux := http.NewServeMux()
	governance.Register(mux)
	(&OpenAICompatibleHandlers{Gateway: gw, Bundle: governedTestBundle(model), Governance: governance}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)
	return srv
}

func governedTestBundle(model string) modelgateway.ModelPolicyBundleDoc {
	return modelgateway.ModelPolicyBundleDoc{Profiles: []modelgateway.ModelProfileDoc{{
		Profile: "governed-test",
		Candidates: []modelgateway.CandidateDoc{{
			Provider: "fake-compute", Model: model,
			Modality: modelgateway.ModalityText, InferenceClass: modelgateway.InferenceClassCloud, Priority: 0,
		}},
	}}}
}

// postChatCompletionWithHeaders is postChatCompletion plus the governance headers — which is the
// whole subject here, so they cannot be baked into the helper.
func postChatCompletionWithHeaders(t *testing.T, srv *httptest.Server, body map[string]any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(authorize(req))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, parsed
}

// aeonBlock reads the governance report, failing when there is none — its ABSENCE is the old
// behaviour, so "no block" must never read as "nothing to report".
func aeonBlock(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	aeon, ok := body["aeon"].(map[string]any)
	if !ok {
		t.Fatalf("the response carries no `aeon` governance block, so a caller cannot tell whether "+
			"this call was governed: %v", body)
	}
	return aeon
}

func reasonAt(reasons []any, i int) string {
	if i >= len(reasons) {
		return ""
	}
	s, _ := reasons[i].(string)
	return s
}

// TestIdempotencyHeaderIsNotBilledTwiceThroughTheOpenAISurface is A-3's other acceptance condition,
// and it needs the real platform for the reason OBS-006's test already states: the replay headers
// come from the platform and no fixture can produce them honestly. A double that "replays" is a
// double asserting that our code handles a reply the double was written to send.
//
// Run by `make test-vrt-aeon-003`.
func TestIdempotencyHeaderIsNotBilledTwiceThroughTheOpenAISurface(t *testing.T) {
	ctx := context.Background()
	client, model := realPrometheusClientOrSkip(t)

	s := newAPITestStore(t)
	ledger := s.FinOpsLedgerFor("default")
	pricing := finops.NewPricingTable([]finops.Rate{{
		Provider: "prometheus_inference", Model: model, CostModel: "token_based",
		InputPerMillionUSD: 0.2, OutputPerMillionUSD: 0.6,
	}})
	gw := modelgateway.New()
	gw.RegisterProvider(prometheusinference.Name, &prometheusinference.Adapter{Client: client, Model: model})
	governance := &ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: s, Agents: s}
	mux := http.NewServeMux()
	governance.Register(mux)
	(&OpenAICompatibleHandlers{
		Gateway: gw,
		Bundle: modelgateway.ModelPolicyBundleDoc{Profiles: []modelgateway.ModelProfileDoc{{
			Profile: "idempotency-test",
			Candidates: []modelgateway.CandidateDoc{{
				Provider: prometheusinference.Name, Model: model,
				Modality: modelgateway.ModalityText, InferenceClass: modelgateway.InferenceClassLocal, Priority: 0,
			}},
			// Required, and the first run of this test proved it is enforced on THIS endpoint too: a
			// local-inference candidate with no declared exception was refused 403 aeon_policy_denied.
			// The harness recognises no provider as in-network by name (MDL-017) — the profile says so
			// or nothing does.
			LocalInference: &modelgateway.LocalInferenceException{
				Environment: "test", AllowedProviders: []string{prometheusinference.Name},
			},
		}}},
		Governance: governance,
	}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)

	runID := "openai-idem-run-" + randSuffix(t)
	key := "aeon-vrt003-" + randSuffix(t)
	call := func() (int, map[string]any) {
		return postChatCompletionWithHeaders(t, srv, map[string]any{
			"model":      "idempotency-test",
			"messages":   []any{map[string]any{"role": "user", "content": "Responde solo: ok"}},
			"max_tokens": 20,
		}, map[string]string{
			RunIDHeader:            runID,
			IdempotencyKeyHeader:   key,
			AgentManifestRefHeader: "no-such-agent@0.1.0",
		})
	}

	status, first := call()
	if status != http.StatusOK {
		t.Fatalf("first call: status=%d body=%v", status, first)
	}
	firstAeon := aeonBlock(t, first)
	if idempotent, _ := firstAeon["idempotent"].(bool); !idempotent {
		t.Fatalf("the first call does not report that it was sent with an idempotency key: %v", firstAeon)
	}
	if _, replayed := firstAeon["idempotent_replay_of"]; replayed {
		t.Fatalf("the first call under a fresh key came back as a replay: %v", firstAeon)
	}
	spendAfterFirst, err := ledger.SpendForRun(ctx, runID)
	if err != nil {
		t.Fatalf("SpendForRun: %v", err)
	}
	if spendAfterFirst.Tokens <= 0 {
		t.Fatalf("the first call recorded %d tokens against the run — with no cost recorded there is "+
			"nothing for the second call to avoid charging twice", spendAfterFirst.Tokens)
	}

	status, second := call()
	if status != http.StatusOK {
		t.Fatalf("second call: status=%d body=%v", status, second)
	}
	secondAeon := aeonBlock(t, second)
	replayOf, _ := secondAeon["idempotent_replay_of"].(string)
	if replayOf == "" {
		t.Fatal("the identical second call under the same Idempotency-Key was not reported as a replay — " +
			"either the header never reached the provider, or this platform no longer replays")
	}
	if cost := secondAeon["cost_usd"]; cost != float64(0) {
		t.Errorf("the replay reports cost_usd=%v; the platform charged nothing for it", cost)
	}

	// THE ASSERTION THE REQUEST ACTUALLY ASKS FOR: the ledger, not the response. The replay's own row
	// exists — a call that leaves no row cannot be audited (OBS-008) — and it carries zeroes, so the
	// run's total is unchanged.
	spendAfterSecond, err := ledger.SpendForRun(ctx, runID)
	if err != nil {
		t.Fatalf("SpendForRun: %v", err)
	}
	if spendAfterSecond.Tokens != spendAfterFirst.Tokens {
		t.Fatalf("the run's recorded tokens went from %d to %d across an idempotent replay — the same "+
			"generation was billed twice", spendAfterFirst.Tokens, spendAfterSecond.Tokens)
	}
	if spendAfterSecond.CostUSD != spendAfterFirst.CostUSD {
		t.Fatalf("the run's recorded cost went from %v to %v across an idempotent replay",
			spendAfterFirst.CostUSD, spendAfterSecond.CostUSD)
	}
	if spendAfterSecond.ModelCalls != spendAfterFirst.ModelCalls+1 {
		t.Errorf("model_calls = %d, want %d: a replay is a real call that consumed nothing, so it "+
			"counts as a call and not as spend", spendAfterSecond.ModelCalls, spendAfterFirst.ModelCalls+1)
	}
	t.Logf("run %s: %d tokens / $%.6f before and after the replay of %s",
		runID, spendAfterSecond.Tokens, spendAfterSecond.CostUSD, replayOf)
}
