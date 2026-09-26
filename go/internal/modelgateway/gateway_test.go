package modelgateway

import (
	"context"
	"errors"
	"testing"
)

// fakeProvider is a minimal providers.Provider test double — no HTTP, just a scripted outcome.
// Real adapter conformance (does a real HTTP client behave correctly) is each adapter's own test;
// this package only needs to prove routing/fallback logic, which is provider-agnostic by design.
type fakeProvider struct {
	fail      bool
	responded map[string]any
}

func (f *fakeProvider) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	if f.fail {
		return nil, errors.New("simulated provider failure")
	}
	// A USABLE normalized response by default (MDL-015). The gateway now treats a response with no
	// content and no tool calls as this candidate failing, so a double returning a bare
	// {"model": ...} is no longer testing "this candidate served the call" — it is testing the
	// unusable-answer path. Every routing assertion below depends on a candidate succeeding, so the
	// default has to be a real answer; `responded` still overrides it for a test that wants otherwise.
	out := map[string]any{
		"model": renderedContext["model"],
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "ok"},
		}},
	}
	for k, v := range f.responded {
		out[k] = v
	}
	return out, nil
}
func (f *fakeProvider) CachingCapability() string { return "none" }
func (f *fakeProvider) CostModel() string         { return "token_based" }

func TestModelGatewayRoutingFallback(t *testing.T) {
	t.Run("falls back to the next candidate on failure", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("primary", &fakeProvider{fail: true})
		// No `responded` override: the fallback has to return a USABLE answer for this test to mean
		// "the fallback served the call". It used to pass {"choices": ["ok"]} — a string, not a choice
		// object — which the gateway accepted because it never looked inside. It does now (MDL-015).
		gw.RegisterProvider("fallback", &fakeProvider{})

		result, err := gw.Decide(context.Background(), []Candidate{
			{Provider: "primary", Model: "m1", Priority: 0},
			{Provider: "fallback", Model: "m2", Priority: 1},
		}, map[string]any{"messages": []any{}}, "")

		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if result.ProviderUsed != "fallback" {
			t.Fatalf("ProviderUsed = %q, want fallback", result.ProviderUsed)
		}
		if len(result.Attempts) != 2 {
			t.Fatalf("expected 2 attempts (primary failed, fallback succeeded), got %d: %+v", len(result.Attempts), result.Attempts)
		}
		if result.Attempts[0].Err == "" {
			t.Fatalf("expected the primary attempt to record its failure")
		}
		if result.Attempts[1].Err != "" {
			t.Fatalf("expected the fallback attempt to record no error, got %q", result.Attempts[1].Err)
		}
	})

	t.Run("tries candidates in priority order regardless of slice order", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("low_priority", &fakeProvider{responded: map[string]any{"which": "low"}})
		gw.RegisterProvider("high_priority", &fakeProvider{responded: map[string]any{"which": "high"}})

		// Deliberately listed out of priority order.
		result, err := gw.Decide(context.Background(), []Candidate{
			{Provider: "low_priority", Model: "m1", Priority: 5},
			{Provider: "high_priority", Model: "m2", Priority: 0},
		}, map[string]any{}, "")

		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if result.ProviderUsed != "high_priority" {
			t.Fatalf("ProviderUsed = %q, want high_priority (lowest Priority value tries first)", result.ProviderUsed)
		}
	})

	t.Run("returns ErrAllCandidatesFailed with the full attempt log when everything fails", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("a", &fakeProvider{fail: true})
		gw.RegisterProvider("b", &fakeProvider{fail: true})

		_, err := gw.Decide(context.Background(), []Candidate{
			{Provider: "a", Model: "m1", Priority: 0},
			{Provider: "b", Model: "m2", Priority: 1},
		}, map[string]any{}, "")

		if !errors.Is(err, ErrAllCandidatesFailed) {
			t.Fatalf("expected ErrAllCandidatesFailed, got %v", err)
		}
	})

	t.Run("an unregistered provider name fails over to the next candidate, not a panic", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("real", &fakeProvider{responded: map[string]any{"ok": true}})

		result, err := gw.Decide(context.Background(), []Candidate{
			{Provider: "typo_provider", Model: "m1", Priority: 0},
			{Provider: "real", Model: "m2", Priority: 1},
		}, map[string]any{}, "")

		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if result.ProviderUsed != "real" {
			t.Fatalf("ProviderUsed = %q, want real", result.ProviderUsed)
		}
	})

	t.Run("data_sensitivity=restricted only ever tries an in-network candidate, even if it's lower priority", func(t *testing.T) {
		// The provider names here are arbitrary on purpose: what makes a candidate eligible is the
		// profile declaring it in-network, not what it is called. Before MDL-017 this test passed
		// for the wrong reason — the routing core recognised one specific name.
		gw := New()
		gw.RegisterProvider("some-cloud", &fakeProvider{responded: map[string]any{"leaked": true}})
		gw.RegisterProvider("some-in-network", &fakeProvider{responded: map[string]any{"local": true}})

		result, err := gw.Decide(context.Background(), []Candidate{
			{Provider: "some-cloud", Model: "m1", Priority: 0}, // would win on priority alone
			{Provider: "some-in-network", Model: "m2", Priority: 1, InNetwork: true},
		}, map[string]any{}, "restricted")

		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if result.ProviderUsed != "some-in-network" {
			t.Fatalf("ProviderUsed = %q, want the in-network one — restricted data must never even attempt a candidate outside the network", result.ProviderUsed)
		}
		if len(result.Attempts) != 1 {
			t.Fatalf("expected exactly 1 attempt (anthropic must never be tried at all for restricted data), got %+v", result.Attempts)
		}
	})

	t.Run("data_sensitivity=restricted with nothing declared in-network fails clearly", func(t *testing.T) {
		// Fail closed: an undeclared topology is not an open one. A candidate that simply never got
		// the declaration must not serve restricted data by default.
		gw := New()
		gw.RegisterProvider("anthropic", &fakeProvider{responded: map[string]any{}})

		_, err := gw.Decide(context.Background(), []Candidate{
			{Provider: "anthropic", Model: "m1", Priority: 0},
		}, map[string]any{}, "restricted")

		if !errors.Is(err, ErrNoRestrictedCandidate) {
			t.Fatalf("expected ErrNoRestrictedCandidate, got %v", err)
		}
	})
}

// fakeQualityGate reports exactly the (provider, model) pairs listed in degraded as degraded — a
// minimal QualityGate test double, mirroring fakeProvider's own style.
type fakeQualityGate struct {
	degraded map[string]bool
}

func (f *fakeQualityGate) IsDegraded(ctx context.Context, provider, model string) bool {
	return f.degraded[provider+"/"+model]
}

// TestModelGatewayQualityAwareRouting is MDL-002's acceptance test: a real routing decision changes
// — the higher-priority candidate is skipped entirely, never even attempted — once its quality
// score reports as degraded.
func TestModelGatewayQualityAwareRouting(t *testing.T) {
	t.Run("a degraded higher-priority candidate is skipped, never attempted", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("degraded-provider", &fakeProvider{responded: map[string]any{}})
		gw.RegisterProvider("healthy-provider", &fakeProvider{responded: map[string]any{}})
		gw.Quality = &fakeQualityGate{degraded: map[string]bool{"degraded-provider/m1": true}}

		result, err := gw.Decide(context.Background(), []Candidate{
			{Provider: "degraded-provider", Model: "m1", Priority: 0},
			{Provider: "healthy-provider", Model: "m2", Priority: 1},
		}, map[string]any{"messages": []any{}}, "")

		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if result.ProviderUsed != "healthy-provider" {
			t.Fatalf("ProviderUsed = %q, want healthy-provider — the degraded, higher-priority candidate should have been skipped", result.ProviderUsed)
		}
		if len(result.Attempts) != 2 {
			t.Fatalf("expected 2 attempts (the skip is recorded), got %+v", result.Attempts)
		}
		if result.Attempts[0].Provider != "degraded-provider" || result.Attempts[0].Err == "" {
			t.Fatalf("expected the first attempt to record the degraded candidate as skipped, got %+v", result.Attempts[0])
		}
	})

	t.Run("without a QualityGate configured, routing is unaffected (nil-safe)", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("only-provider", &fakeProvider{responded: map[string]any{}})

		result, err := gw.Decide(context.Background(), []Candidate{
			{Provider: "only-provider", Model: "m1", Priority: 0},
		}, map[string]any{"messages": []any{}}, "")

		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if result.ProviderUsed != "only-provider" {
			t.Fatalf("ProviderUsed = %q, want only-provider", result.ProviderUsed)
		}
	})

	t.Run("when every candidate is degraded, the error still reports every skip", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("degraded-provider", &fakeProvider{responded: map[string]any{}})
		gw.Quality = &fakeQualityGate{degraded: map[string]bool{"degraded-provider/m1": true}}

		_, err := gw.Decide(context.Background(), []Candidate{
			{Provider: "degraded-provider", Model: "m1", Priority: 0},
		}, map[string]any{"messages": []any{}}, "")

		if !errors.Is(err, ErrAllCandidatesFailed) {
			t.Fatalf("expected ErrAllCandidatesFailed, got %v", err)
		}
	})
}
