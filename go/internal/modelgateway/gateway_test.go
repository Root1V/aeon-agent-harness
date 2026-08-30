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
	out := map[string]any{"model": renderedContext["model"]}
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
		gw.RegisterProvider("fallback", &fakeProvider{responded: map[string]any{"choices": []any{"ok"}}})

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

	t.Run("data_sensitivity=restricted only ever tries prometheus_inference, even if it's lower priority", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("anthropic", &fakeProvider{responded: map[string]any{"leaked": true}})
		gw.RegisterProvider("prometheus_inference", &fakeProvider{responded: map[string]any{"local": true}})

		result, err := gw.Decide(context.Background(), []Candidate{
			{Provider: "anthropic", Model: "m1", Priority: 0}, // would win on priority alone
			{Provider: "prometheus_inference", Model: "m2", Priority: 1},
		}, map[string]any{}, "restricted")

		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if result.ProviderUsed != "prometheus_inference" {
			t.Fatalf("ProviderUsed = %q, want prometheus_inference — restricted data must never even attempt a cloud candidate", result.ProviderUsed)
		}
		if len(result.Attempts) != 1 {
			t.Fatalf("expected exactly 1 attempt (anthropic must never be tried at all for restricted data), got %+v", result.Attempts)
		}
	})

	t.Run("data_sensitivity=restricted with no local candidate configured fails clearly", func(t *testing.T) {
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
