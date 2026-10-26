package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/providers"
)

// throttlingProvider refuses with the provider-agnostic wait, the way an adapter translates its
// platform's typed refusal (MDL-024).
type throttlingProvider struct {
	wait  float64
	scope string
}

func (p *throttlingProvider) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	model, _ := renderedContext["model"].(string)
	return nil, &providers.Throttled{
		Provider: "fake-throttled", Model: model, Scope: p.scope,
		RetryAfterSeconds: &p.wait, Err: errors.New("429 from the platform"),
	}
}
func (p *throttlingProvider) CachingCapability() string { return "none" }
func (p *throttlingProvider) CostModel() string         { return "token_based" }

// brokenProvider fails for a reason no wait fixes.
type brokenProvider struct{}

func (brokenProvider) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	return nil, errors.New("connection refused")
}
func (brokenProvider) CachingCapability() string { return "none" }
func (brokenProvider) CostModel() string         { return "token_based" }

// MDL-024 on the two surfaces: a pool that only asked us to wait answers 429 with the wait, and a pool
// where anything failed for another reason stays 502.
//
// THE SECOND HALF IS THE CORRECTNESS CONDITION. With one candidate down for a reason retrying will not
// fix, the call failed — and answering 429 there sends the caller back into a cascade that is still
// broken, on a promise nobody made. So the surfaces answer 429 only when EVERY attempt was a wait, and
// the wait is the MINIMUM: the soonest moment at which anything in the pool could work.
func TestEveryCandidateRateLimitedAnswers429(t *testing.T) {
	s := newAPITestStore(t)
	model := "mdl024-" + randSuffix(t)

	newServer := func(register func(*modelgateway.Gateway)) *httptest.Server {
		gw := modelgateway.New()
		register(gw)
		pricing := finops.NewPricingTable([]finops.Rate{
			{Provider: "fake-throttled", Model: model, CostModel: "token_based"},
			{Provider: "fake-broken", Model: model, CostModel: "token_based"},
		})
		governance := &ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: s, Agents: s}
		mux := http.NewServeMux()
		governance.Register(mux)
		(&OpenAICompatibleHandlers{
			Gateway: gw, Bundle: throttleTestBundle(model), Governance: governance,
		}).Register(mux)
		srv := httptest.NewServer(authWrap(t, mux))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("both candidates limited: 429 with the soonest wait on both surfaces", func(t *testing.T) {
		srv := newServer(func(gw *modelgateway.Gateway) {
			gw.RegisterProvider("fake-throttled", &throttlingProvider{wait: 12, scope: "chat_completions"})
		})

		// /decide, which is the surface Synaptum's transport reads.
		status, body := postDecide(t, srv, decideRequest{
			Candidates: []decideCandidate{
				{Provider: "fake-throttled", Model: model, Priority: 0},
				{Provider: "fake-throttled", Model: model, Priority: 1},
			},
			RenderedContext: map[string]any{"messages": []any{}},
		})
		if status != http.StatusTooManyRequests {
			t.Fatalf("POST /decide = %d, want 429 — a pool that only asked us to wait is not a pool that "+
				"failed, and 502 makes the caller retry now with the same request: %v", status, body)
		}
		if retryable, _ := body["retryable"].(bool); !retryable {
			t.Errorf("retryable = %v, want true: retryability travels in the error taxonomy and not in a "+
				"policy disposition, which is why a budget stop says terminate_run and this does not", body["retryable"])
		}
		if wait, _ := body["retry_after"].(float64); wait != 12 {
			t.Errorf("retry_after = %v, want 12", body["retry_after"])
		}
		limited, _ := body["rate_limited"].([]any)
		if len(limited) != 2 {
			t.Fatalf("rate_limited lists %d candidates, want 2 — being told to wait without being told "+
				"what for is not actionable: %v", len(limited), body)
		}
		first, _ := limited[0].(map[string]any)
		if first["scope"] != "chat_completions" {
			t.Errorf("scope = %v, want chat_completions", first["scope"])
		}

		// And the OpenAI-compatible surface, in OpenAI's own vocabulary so an existing client's retry
		// logic fires without learning anything of ours.
		status, parsed := postChatCompletionWithHeaders(t, srv, map[string]any{
			"model":    "throttle-test",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
		if status != http.StatusTooManyRequests {
			t.Fatalf("POST /v1/chat/completions = %d, want 429: %v", status, parsed)
		}
		errObj, _ := parsed["error"].(map[string]any)
		if errObj["type"] != "rate_limit_error" {
			t.Errorf("error.type = %v, want rate_limit_error — OpenAI's own type, so a client that "+
				"understands only that envelope still retries correctly", errObj["type"])
		}
		aeon := aeonBlock(t, parsed)
		if wait, _ := aeon["retry_after"].(float64); wait != 12 {
			t.Errorf("aeon.retry_after = %v, want 12", aeon["retry_after"])
		}
	})

	t.Run("the soonest wait wins when they differ", func(t *testing.T) {
		srv := newServer(func(gw *modelgateway.Gateway) {
			gw.RegisterProvider("fake-throttled", &throttlingProvider{wait: 30, scope: "chat_completions"})
			gw.RegisterProvider("fake-broken", &throttlingProvider{wait: 5, scope: "default"})
		})
		status, body := postDecide(t, srv, decideRequest{
			Candidates: []decideCandidate{
				{Provider: "fake-throttled", Model: model, Priority: 0},
				{Provider: "fake-broken", Model: model, Priority: 1},
			},
			RenderedContext: map[string]any{"messages": []any{}},
		})
		if status != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429: %v", status, body)
		}
		if wait, _ := body["retry_after"].(float64); wait != 5 {
			t.Errorf("retry_after = %v, want 5 — the minimum is the soonest moment anything in the pool "+
				"could work, and the maximum would make the caller wait on the slowest window", body["retry_after"])
		}
	})

	t.Run("one candidate broken for another reason stays 502", func(t *testing.T) {
		srv := newServer(func(gw *modelgateway.Gateway) {
			gw.RegisterProvider("fake-throttled", &throttlingProvider{wait: 12, scope: "chat_completions"})
			gw.RegisterProvider("fake-broken", brokenProvider{})
		})
		status, body := postDecide(t, srv, decideRequest{
			Candidates: []decideCandidate{
				{Provider: "fake-throttled", Model: model, Priority: 0},
				{Provider: "fake-broken", Model: model, Priority: 1},
			},
			RenderedContext: map[string]any{"messages": []any{}},
		})
		if status != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502: one candidate failed for a reason no wait fixes, so telling "+
				"the caller to retry would be a promise nobody made: %v", status, body)
		}
	})
}

func throttleTestBundle(model string) modelgateway.ModelPolicyBundleDoc {
	return modelgateway.ModelPolicyBundleDoc{Profiles: []modelgateway.ModelProfileDoc{{
		Profile: "throttle-test",
		Candidates: []modelgateway.CandidateDoc{
			{Provider: "fake-throttled", Model: model, Modality: modelgateway.ModalityText,
				InferenceClass: modelgateway.InferenceClassCloud, Priority: 0},
		},
	}}}
}
