package openaicompatible

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newFakeServer(t *testing.T, requireAuth bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if requireAuth && r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := openAICompatibleResponse{ID: "cmpl-fake", Model: body["model"].(string)}
		resp.Choices = []openAICompatibleChoice{{Index: 0, FinishReason: "stop"}}
		resp.Choices[0].Message.Role = "assistant"
		resp.Choices[0].Message.Content = "pong"
		// Usage became a POINTER in FND-004, so that nil can answer "there was no usage object at all" —
		// the question that decides whether counters are reported or derived from timings. This double
		// builds its body from the production struct, so it has to allocate it; leaving it nil would serve
		// a body with no usage, which is a different case than this test means to cover.
		resp.Usage = &openAICompatibleUsage{}
		resp.Usage.PromptTokens = tokPtr(4)
		resp.Usage.CompletionTokens = tokPtr(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAICompatibleAdapterWorksWithoutAnAPIKey(t *testing.T) {
	// The whole point of this adapter: a self-hosted server (vLLM/Ollama default config) that
	// needs no authentication at all must still work.
	srv := newFakeServer(t, false)
	adapter := &Adapter{BaseURL: srv.URL}

	result, err := adapter.Decide(t.Context(), map[string]any{
		"model":    "llama-3-70b",
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	choices := result["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "pong" {
		t.Fatalf("content = %v, want pong", message["content"])
	}
}

func TestOpenAICompatibleAdapterSendsBearerTokenWhenConfigured(t *testing.T) {
	srv := newFakeServer(t, true)
	adapter := &Adapter{BaseURL: srv.URL, APIKey: "local-token"}

	_, err := adapter.Decide(t.Context(), map[string]any{
		"model":    "llama-3-70b",
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
}

func TestOpenAICompatibleAdapterRequiresBaseURL(t *testing.T) {
	adapter := &Adapter{}
	_, err := adapter.Decide(t.Context(), map[string]any{"model": "x", "messages": []any{}})
	if err == nil {
		t.Fatal("expected an error when BaseURL is not configured — there is no sensible default for a self-hosted server")
	}
}

func TestOpenAICompatibleAdapterCachingAndCostModel(t *testing.T) {
	a := &Adapter{}
	if a.CachingCapability() != "none" {
		t.Fatalf("CachingCapability() = %q", a.CachingCapability())
	}
	if a.CostModel() != "compute_based" {
		t.Fatalf("CostModel() = %q", a.CostModel())
	}
}

// tokPtr returns a pointer to n. The wire counters are pointers since MDL-014, so setting one says "the
// upstream reported this" and leaving it nil says "it reported nothing" — a case the corpus now covers
// on purpose rather than by accident.
func tokPtr(n int) *int { return &n }
