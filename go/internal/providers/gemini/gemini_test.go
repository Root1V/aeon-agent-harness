package gemini

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newFakeGeminiServer(t *testing.T, wantAPIKey string) (*httptest.Server, *geminiRequest, *string) {
	t.Helper()
	var captured geminiRequest
	var requestedModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path is /v1beta/models/{model}:generateContent — extract {model}.
		const prefix = "/v1beta/models/"
		if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, ":generateContent") {
			http.NotFound(w, r)
			return
		}
		requestedModel = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), ":generateContent")

		if r.Header.Get("x-goog-api-key") != wantAPIKey {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(geminiError{})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		resp := geminiResponse{}
		resp.Candidates = []struct {
			Content      geminiContent `json:"content"`
			FinishReason string        `json:"finishReason"`
		}{
			{Content: geminiContent{Role: "model", Parts: []geminiPart{{Text: "pong"}}}, FinishReason: "STOP"},
		}
		resp.UsageMetadata.PromptTokenCount = 8
		resp.UsageMetadata.CandidatesTokenCount = 1
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &captured, &requestedModel
}

func TestGeminiAdapterDecideTranslatesRolesAndSystemInstruction(t *testing.T) {
	srv, captured, requestedModel := newFakeGeminiServer(t, "test-key")
	adapter := &Adapter{APIKey: "test-key", BaseURL: srv.URL}

	result, err := adapter.Decide(t.Context(), map[string]any{
		"model": "gemini-3-pro",
		"messages": []any{
			map[string]any{"role": "system", "content": "be terse"},
			map[string]any{"role": "user", "content": "ping"},
			map[string]any{"role": "assistant", "content": "previous reply"},
		},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if *requestedModel != "gemini-3-pro" {
		t.Fatalf("model in URL path = %q, want gemini-3-pro", *requestedModel)
	}
	if captured.SystemInstruction == nil || captured.SystemInstruction.Parts[0].Text != "be terse" {
		t.Fatalf("expected system message extracted into systemInstruction, got %+v", captured.SystemInstruction)
	}
	if len(captured.Contents) != 2 {
		t.Fatalf("expected 2 non-system contents, got %+v", captured.Contents)
	}
	if captured.Contents[0].Role != "user" {
		t.Fatalf("Contents[0].Role = %q, want user", captured.Contents[0].Role)
	}
	if captured.Contents[1].Role != "model" {
		t.Fatalf("Contents[1].Role = %q, want model (assistant must map to Gemini's 'model' role)", captured.Contents[1].Role)
	}

	choices := result["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "pong" {
		t.Fatalf("content = %v, want pong", message["content"])
	}
	usage := result["usage"].(map[string]any)
	if usage["prompt_tokens"] != 8 || usage["completion_tokens"] != 1 || usage["total_tokens"] != 9 {
		t.Fatalf("usage not translated correctly: %v", usage)
	}
}

func TestGeminiAdapterAPIKeyNeverAppearsInTheURL(t *testing.T) {
	// The documented Gemini contract puts the key in a ?key= query param; this adapter must use
	// the x-goog-api-key header instead, so a secret never ends up in a URL that gets logged.
	srv, _, _ := newFakeGeminiServer(t, "super-secret-key")
	adapter := &Adapter{APIKey: "super-secret-key", BaseURL: srv.URL}

	var sawKeyInURL bool
	originalTransport := http.DefaultTransport
	_ = originalTransport
	adapter.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.RawQuery, "super-secret-key") {
			sawKeyInURL = true
		}
		return http.DefaultTransport.RoundTrip(req)
	})}

	_, err := adapter.Decide(t.Context(), map[string]any{
		"model":    "gemini-3-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if sawKeyInURL {
		t.Fatal("the API key must never appear in the request URL")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestGeminiAdapterFailsOnWrongAPIKey(t *testing.T) {
	srv, _, _ := newFakeGeminiServer(t, "correct-key")
	adapter := &Adapter{APIKey: "wrong-key", BaseURL: srv.URL}

	_, err := adapter.Decide(t.Context(), map[string]any{
		"model":    "gemini-3-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error for a rejected API key")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected the error to mention the 401 status, got: %v", err)
	}
}

func TestGeminiAdapterCachingAndCostModel(t *testing.T) {
	a := &Adapter{}
	if a.CachingCapability() != "explicit_breakpoints" {
		t.Fatalf("CachingCapability() = %q", a.CachingCapability())
	}
	if a.CostModel() != "token_based" {
		t.Fatalf("CostModel() = %q", a.CostModel())
	}
}
