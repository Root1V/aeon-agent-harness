package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newFakeOpenAIServer(t *testing.T, wantAPIKey string) (*httptest.Server, *map[string]any) {
	t.Helper()
	captured := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+wantAPIKey {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(openAIError{})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		resp := openAIResponse{ID: "chatcmpl-fake", Model: captured["model"].(string)}
		resp.Choices = []openAIChoice{{Index: 0, FinishReason: "stop"}}
		resp.Choices[0].Message.Role = "assistant"
		resp.Choices[0].Message.Content = "pong"
		resp.Usage.PromptTokens = 5
		resp.Usage.CompletionTokens = 2
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func TestOpenAIAdapterDecideNormalizesResponse(t *testing.T) {
	srv, captured := newFakeOpenAIServer(t, "test-key")
	adapter := &Adapter{APIKey: "test-key", BaseURL: srv.URL}

	result, err := adapter.Decide(t.Context(), map[string]any{
		"model":    "gpt-5",
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if (*captured)["model"] != "gpt-5" {
		t.Fatalf("request not sent through correctly: %v", *captured)
	}

	choices := result["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "pong" {
		t.Fatalf("content = %v, want pong", message["content"])
	}
	usage := result["usage"].(map[string]any)
	if usage["prompt_tokens"] != 5 || usage["completion_tokens"] != 2 || usage["total_tokens"] != 7 {
		t.Fatalf("usage not translated correctly: %v", usage)
	}
}

func TestOpenAIAdapterFailsOnWrongAPIKey(t *testing.T) {
	srv, _ := newFakeOpenAIServer(t, "correct-key")
	adapter := &Adapter{APIKey: "wrong-key", BaseURL: srv.URL}

	_, err := adapter.Decide(t.Context(), map[string]any{
		"model":    "gpt-5",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error for a rejected API key")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected the error to mention the 401 status, got: %v", err)
	}
}

func TestOpenAIAdapterCachingAndCostModel(t *testing.T) {
	a := &Adapter{}
	if a.CachingCapability() != "automatic_prefix" {
		t.Fatalf("CachingCapability() = %q", a.CachingCapability())
	}
	if a.CostModel() != "token_based" {
		t.Fatalf("CostModel() = %q", a.CostModel())
	}
}
