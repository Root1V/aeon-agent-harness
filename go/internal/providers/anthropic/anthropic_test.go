package anthropic

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAnthropicServer implements the documented Messages API contract: POST /v1/messages,
// x-api-key + anthropic-version headers required, request/response shapes matching Anthropic's
// real API — not a superficial mock.
func newFakeAnthropicServer(t *testing.T, wantAPIKey string) (*httptest.Server, *anthropicRequest) {
	t.Helper()
	var captured anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-api-key") != wantAPIKey {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(anthropicError{})
			return
		}
		if r.Header.Get("anthropic-version") == "" {
			http.Error(w, "missing anthropic-version header", http.StatusBadRequest)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		resp := anthropicResponse{
			ID:         "msg_fake123",
			Model:      captured.Model,
			Content:    []anthropicContentBlock{{Type: "text", Text: "pong"}},
			StopReason: "end_turn",
		}
		resp.Usage.InputTokens = 10
		resp.Usage.OutputTokens = 3
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func TestAnthropicAdapterDecideNormalizesRequestAndResponse(t *testing.T) {
	srv, captured := newFakeAnthropicServer(t, "test-key")
	adapter := &Adapter{APIKey: "test-key", BaseURL: srv.URL}

	result, err := adapter.Decide(t.Context(), map[string]any{
		"model": "claude-opus-5",
		"messages": []any{
			map[string]any{"role": "system", "content": "be terse"},
			map[string]any{"role": "user", "content": "ping"},
		},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// Request translation: system-role message extracted into the top-level `system` field, not
	// left in `messages` — Anthropic's API has no "system" role in messages.
	if captured.System != "be terse" {
		t.Fatalf("System = %q, want 'be terse'", captured.System)
	}
	if len(captured.Messages) != 1 || captured.Messages[0].Role != "user" {
		t.Fatalf("expected exactly one non-system message, got %+v", captured.Messages)
	}
	if captured.MaxTokens != defaultMaxTokens {
		t.Fatalf("MaxTokens = %d, want the default %d (Anthropic requires this field)", captured.MaxTokens, defaultMaxTokens)
	}

	// Response translation: normalized shape, not Anthropic's raw content-blocks shape.
	choices, ok := result["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("expected a normalized choices[] array, got %v", result["choices"])
	}
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "pong" {
		t.Fatalf("content = %v, want 'pong'", message["content"])
	}
	usage := result["usage"].(map[string]any)
	if usage["prompt_tokens"] != 10 || usage["completion_tokens"] != 3 || usage["total_tokens"] != 13 {
		t.Fatalf("usage not translated correctly: %v", usage)
	}
}

func TestAnthropicAdapterRespectsExplicitMaxTokens(t *testing.T) {
	srv, captured := newFakeAnthropicServer(t, "test-key")
	adapter := &Adapter{APIKey: "test-key", BaseURL: srv.URL}

	_, err := adapter.Decide(t.Context(), map[string]any{
		"model":      "claude-opus-5",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 42,
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if captured.MaxTokens != 42 {
		t.Fatalf("MaxTokens = %d, want 42", captured.MaxTokens)
	}
}

func TestAnthropicAdapterFailsOnWrongAPIKey(t *testing.T) {
	srv, _ := newFakeAnthropicServer(t, "correct-key")
	adapter := &Adapter{APIKey: "wrong-key", BaseURL: srv.URL}

	_, err := adapter.Decide(t.Context(), map[string]any{
		"model":    "claude-opus-5",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error for a rejected API key")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected the error to mention the 401 status, got: %v", err)
	}
}

func TestAnthropicAdapterCachingAndCostModel(t *testing.T) {
	a := &Adapter{}
	if a.CachingCapability() != "explicit_breakpoints" {
		t.Fatalf("CachingCapability() = %q", a.CachingCapability())
	}
	if a.CostModel() != "token_based" {
		t.Fatalf("CostModel() = %q", a.CostModel())
	}
}
