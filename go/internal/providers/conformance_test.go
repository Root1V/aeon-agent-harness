package providers_test

// TestProviderConformance is F0's minimal provider_conformance suite (roadmap.md MDL-003..007's
// DONE criterion, and F0's own phase-exit "test_provider_parity"). The full EVAL-002/F2 version
// will run richer cases (tool calling, structured output, constraint respect, long context,
// injection rejection) against every Released agent; this version proves the one property that
// matters for F0 itself: the SAME representative request, sent through each of the five real
// adapters against a fake server implementing that provider's actual documented contract, comes
// back as providers.NormalizedChatResponse — the identical shape regardless of which provider
// actually served it. A caller (the Model Gateway, MDL-001) never needs a provider-specific case.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/providers"
	"github.com/aeon-ai/aeon/go/internal/providers/anthropic"
	"github.com/aeon-ai/aeon/go/internal/providers/gemini"
	"github.com/aeon-ai/aeon/go/internal/providers/openai"
	openaicompatible "github.com/aeon-ai/aeon/go/internal/providers/openai_compatible"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
)

func TestProviderConformance(t *testing.T) {
	cases := []struct {
		name     string
		provider providers.Provider
	}{
		{"anthropic", newConformantAnthropic(t)},
		{"openai", newConformantOpenAI(t)},
		{"gemini", newConformantGemini(t)},
		{"openai_compatible", newConformantOpenAICompatible(t)},
		{"prometheus_inference", newConformantPrometheusInference(t)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.provider.Decide(t.Context(), map[string]any{
				"model":    "conformance-test-model",
				"messages": []any{map[string]any{"role": "user", "content": "ping"}},
			})
			if err != nil {
				t.Fatalf("Decide failed: %v", err)
			}
			assertConformsToNormalizedShape(t, result)

			validCaching := map[string]bool{"automatic_prefix": true, "explicit_breakpoints": true, "none": true}
			if !validCaching[tc.provider.CachingCapability()] {
				t.Errorf("CachingCapability() = %q is not one of the documented values", tc.provider.CachingCapability())
			}
			validCost := map[string]bool{"token_based": true, "compute_based": true}
			if !validCost[tc.provider.CostModel()] {
				t.Errorf("CostModel() = %q is not one of the documented values", tc.provider.CostModel())
			}
		})
	}
}

func assertConformsToNormalizedShape(t *testing.T, result map[string]any) {
	t.Helper()
	choices, ok := result["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("expected a non-empty choices[] array, got %v", result["choices"])
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatalf("choices[0] is not an object: %v", choices[0])
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		t.Fatalf("choices[0].message is not an object: %v", choice["message"])
	}
	if _, ok := message["content"].(string); !ok {
		t.Fatalf("choices[0].message.content is not a string: %v", message["content"])
	}
	if _, ok := message["role"].(string); !ok {
		t.Fatalf("choices[0].message.role is not a string: %v", message["role"])
	}
	usage, ok := result["usage"].(map[string]any)
	if !ok {
		t.Fatalf("expected a usage object, got %v", result["usage"])
	}
	for _, field := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if _, ok := usage[field].(int); !ok {
			t.Fatalf("usage.%s is not an int: %v", field, usage[field])
		}
	}
}

func newConformantAnthropic(t *testing.T) providers.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_conformance", "model": "conformance-test-model",
			"content":     []map[string]any{{"type": "text", "text": "pong"}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 5, "output_tokens": 1},
		})
	}))
	t.Cleanup(srv.Close)
	return &anthropic.Adapter{APIKey: "test-key", BaseURL: srv.URL}
}

func newConformantOpenAI(t *testing.T) providers.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-conformance", "model": "conformance-test-model",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "pong"}}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 1},
		})
	}))
	t.Cleanup(srv.Close)
	return &openai.Adapter{APIKey: "test-key", BaseURL: srv.URL}
}

func newConformantGemini(t *testing.T) providers.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{
				{"content": map[string]any{"role": "model", "parts": []map[string]any{{"text": "pong"}}}, "finishReason": "STOP"},
			},
			"usageMetadata": map[string]any{"promptTokenCount": 5, "candidatesTokenCount": 1},
		})
	}))
	t.Cleanup(srv.Close)
	return &gemini.Adapter{APIKey: "test-key", BaseURL: srv.URL}
}

func newConformantOpenAICompatible(t *testing.T) providers.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-conformance", "model": "conformance-test-model",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "pong"}}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 1},
		})
	}))
	t.Cleanup(srv.Close)
	return &openaicompatible.Adapter{BaseURL: srv.URL}
}

func newConformantPrometheusInference(t *testing.T) providers.Provider {
	t.Helper()
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-conformance-token", "token_type": "bearer", "expires_in": 300,
			"scope": r.Form.Get("scope"),
		})
	}))
	t.Cleanup(authSrv.Close)

	gatewaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-conformance", "model": body["model"],
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "pong"}}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 1},
		})
	}))
	t.Cleanup(gatewaySrv.Close)

	return prometheusinference.New(authSrv.URL, gatewaySrv.URL, "test-client", "test-secret", "inference:read", "conformance-test-model")
}
