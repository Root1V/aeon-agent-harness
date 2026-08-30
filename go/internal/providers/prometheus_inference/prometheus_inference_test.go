package prometheusinference

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// fakeAuthServer implements exactly the documented POST /oauth2/token contract: form-urlencoded
// client_credentials grant in, JSON access_token/token_type/expires_in/scope out. It issues a
// fresh, distinct token string every call (so tests can tell "same token reused" from "new token
// issued" by string identity) and counts how many times it's been hit, so tests can assert on
// caching behavior without timing-based sleeps.
type fakeAuthServer struct {
	srv          *httptest.Server
	callCount    atomic.Int32
	wantClientID string
	wantSecret   string
	expiresIn    int
}

func newFakeAuthServer(t *testing.T, wantClientID, wantSecret string, expiresIn int) *fakeAuthServer {
	t.Helper()
	f := &fakeAuthServer{wantClientID: wantClientID, wantSecret: wantSecret, expiresIn: expiresIn}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth2/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != "client_credentials" {
			http.Error(w, "unsupported grant_type", http.StatusBadRequest)
			return
		}
		if r.Form.Get("client_id") != f.wantClientID || r.Form.Get("client_secret") != f.wantSecret {
			http.Error(w, "invalid client", http.StatusUnauthorized)
			return
		}
		n := f.callCount.Add(1)
		resp := tokenResponse{
			AccessToken: fmt.Sprintf("fake-token-%d", n),
			TokenType:   "bearer",
			ExpiresIn:   f.expiresIn,
			Scope:       r.Form.Get("scope"),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// fakeGatewayServer implements /v1/models (public), /v1/models/mine and /v1/chat/completions
// (both requiring a bearer token). rejectOnce, if set, causes exactly one authenticated request
// bearing that specific token to fail with 401 — used to test the retry-with-a-fresh-token path.
type fakeGatewayServer struct {
	srv        *httptest.Server
	rejectOnce string
	rejectedN  atomic.Int32
	lastAuth   atomic.Value // string
}

func newFakeGatewayServer(t *testing.T) *fakeGatewayServer {
	t.Helper()
	g := &fakeGatewayServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(modelsResponse{Object: "list", Data: []Model{
			{ID: "llama3-8b-q4-local", Object: "model", OwnedBy: "prometheus", ContextLength: 8192, Family: "llama3", Quantization: "q4", Modality: "text"},
		}})
	})
	mux.HandleFunc("GET /v1/models/mine", func(w http.ResponseWriter, r *http.Request) {
		if !g.checkAuth(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(modelsResponse{Object: "list", Data: []Model{
			{ID: "llama3-8b-q4-local", Object: "model", OwnedBy: "prometheus", ContextLength: 8192, Family: "llama3", Quantization: "q4", Modality: "text"},
		}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !g.checkAuth(w, r) {
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-fake",
			"object":  "chat.completion",
			"model":   body["model"],
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "hello"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4},
		})
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGatewayServer) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || auth[:len(prefix)] != prefix {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return false
	}
	token := auth[len(prefix):]
	g.lastAuth.Store(token)
	if g.rejectOnce != "" && token == g.rejectOnce && g.rejectedN.Add(1) == 1 {
		http.Error(w, "token rejected (test-forced, once)", http.StatusUnauthorized)
		return false
	}
	return true
}

const (
	testClientID     = "test-client-id"
	testClientSecret = "test-client-secret"
	testScope        = "inference:read inference:stream model:llama3-8b-q4-local"
)

func newTestAdapter(auth *fakeAuthServer, gateway *fakeGatewayServer) *Adapter {
	return New(auth.srv.URL, gateway.srv.URL, testClientID, testClientSecret, testScope, "llama3-8b-q4-local")
}

func TestPrometheusInferenceListModels(t *testing.T) {
	auth := newFakeAuthServer(t, testClientID, testClientSecret, 60)
	gateway := newFakeGatewayServer(t)
	adapter := newTestAdapter(auth, gateway)

	models, err := adapter.Client.ListModels(t.Context())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "llama3-8b-q4-local" {
		t.Fatalf("unexpected models: %+v", models)
	}
	if auth.callCount.Load() != 0 {
		t.Fatalf("ListModels is public and must not require a token; auth was called %d times", auth.callCount.Load())
	}
}

func TestPrometheusInferenceListMyModelsRequiresAndCachesToken(t *testing.T) {
	auth := newFakeAuthServer(t, testClientID, testClientSecret, 60)
	gateway := newFakeGatewayServer(t)
	adapter := newTestAdapter(auth, gateway)
	ctx := t.Context()

	if _, err := adapter.Client.ListMyModels(ctx); err != nil {
		t.Fatalf("first ListMyModels: %v", err)
	}
	if _, err := adapter.Client.ListMyModels(ctx); err != nil {
		t.Fatalf("second ListMyModels: %v", err)
	}
	if got := auth.callCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 token request across 2 calls within the token's TTL (caching), got %d", got)
	}
}

func TestPrometheusInferenceDecideSendsChatCompletionsShape(t *testing.T) {
	auth := newFakeAuthServer(t, testClientID, testClientSecret, 60)
	gateway := newFakeGatewayServer(t)
	adapter := newTestAdapter(auth, gateway)

	result, err := adapter.Decide(t.Context(), map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if result["model"] != "llama3-8b-q4-local" {
		t.Fatalf("expected Decide to default 'model' to the adapter's configured model, got %v", result["model"])
	}
	choices, ok := result["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("expected an OpenAI-shaped choices[] in the response, got %v", result["choices"])
	}
}

func TestPrometheusInferenceRetriesOnceWithFreshTokenAfter401(t *testing.T) {
	auth := newFakeAuthServer(t, testClientID, testClientSecret, 60)
	gateway := newFakeGatewayServer(t)
	adapter := newTestAdapter(auth, gateway)
	ctx := t.Context()

	// Prime the cache with a first token, then tell the gateway to reject exactly that one token
	// once — simulating it having expired slightly early relative to our clock.
	first, err := adapter.Client.Tokens.Token(ctx)
	if err != nil {
		t.Fatalf("priming token: %v", err)
	}
	gateway.rejectOnce = first

	result, err := adapter.Decide(ctx, map[string]any{"messages": []map[string]any{{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatalf("Decide should succeed after transparently retrying with a fresh token: %v", err)
	}
	choices, ok := result["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("unexpected result after retry: %v", result)
	}
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hello" {
		t.Fatalf("expected the fake gateway's real response content after retry, got %v", message["content"])
	}
	if auth.callCount.Load() != 2 {
		t.Fatalf("expected exactly 2 token requests (initial + forced refresh after 401), got %d", auth.callCount.Load())
	}
	lastUsed, _ := gateway.lastAuth.Load().(string)
	if lastUsed == first {
		t.Fatalf("expected the successful retry to use a NEW token, not the rejected one")
	}
}

func TestPrometheusInferenceRejectsWrongCredentials(t *testing.T) {
	auth := newFakeAuthServer(t, testClientID, testClientSecret, 60)
	gateway := newFakeGatewayServer(t)
	adapter := New(auth.srv.URL, gateway.srv.URL, testClientID, "wrong-secret", testScope, "llama3-8b-q4-local")

	if _, err := adapter.Client.ListMyModels(t.Context()); err == nil {
		t.Fatal("expected an error for a client_secret the auth-service doesn't recognize")
	}
}

func TestPrometheusInferenceCachingCapabilityAndCostModel(t *testing.T) {
	a := &Adapter{}
	if a.CachingCapability() != "none" {
		t.Fatalf("expected CachingCapability() = none, got %q", a.CachingCapability())
	}
	if a.CostModel() != "compute_based" {
		t.Fatalf("expected CostModel() = compute_based, got %q", a.CostModel())
	}
}
