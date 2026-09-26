package prometheusinference_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
)

// This file covers what is OURS after MDL-009, and the shrinking is the point rather than a loss of
// coverage.
//
// Deleted with client.go and auth.go: tests for minting a token, caching it, retrying once with a
// fresh one after a 401, and rejecting bad credentials. Those tested a transport we no longer own, and
// keeping them would mean asserting a dependency's internals — a suite that fails when Axonium
// improves something is worse than no suite.
//
// What replaces them is not nothing, and it matters that it is not: Axonium's own suite, the 24/24
// shared corpus cases they reproduce byte-identically, and Aeon's real-platform tests (OBS-006,
// OBS-007, MDL-015) which exercise auth, replay and usage against the live deployment. That is a
// genuine trade and worth naming: we gave up cheap, hermetic tests of token mechanics for slower tests
// of the whole path, and gained one implementation of the contract instead of two.
//
// What stays here is the mapping between the platform's shape and Aeon's vocabulary, which is ours
// alone. The fake gateway below serves BOTH /oauth2/token and /v1/, because the SDK needs one address
// — which is itself the property MDL-009 verified against the real deployment.

// fakeGateway serves the token endpoint and one canned chat response from a single address.
func fakeGateway(t *testing.T, chatBody map[string]any, headers map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "test-token", "token_type": "Bearer", "expires_in": 600,
				"scope": "inference:read model:test-model",
			})
			return
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chatResponse(content string) map[string]any {
	return map[string]any{
		"model": "test-model",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": content},
		}},
		"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 20, "total_tokens": 32},
	}
}

// TestAdapterNormalizesOverTheSDK is MDL-009's acceptance test: the transport is Axonium's, the
// vocabulary is still Aeon's, and the metadata the SDK surfaces reaches the normalized response.
func TestAdapterNormalizesOverTheSDK(t *testing.T) {
	ctx := context.Background()

	t.Run("a chat response normalizes into Aeon's vocabulary", func(t *testing.T) {
		srv := fakeGateway(t, chatResponse("hola"), map[string]string{
			"X-Request-ID":             "req-1",
			"X-Prometheus-Instance-Id": "qwen3-local-1",
		})
		adapter := prometheusinference.New("", srv.URL, "id", "secret", "inference:read model:test-model", "test-model")

		out, err := adapter.Decide(ctx, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		choices, _ := out["choices"].([]any)
		if len(choices) == 0 {
			t.Fatal("no choices in the normalized response")
		}
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if message["content"] != "hola" {
			t.Errorf("content = %v, want hola", message["content"])
		}
		// Real Go ints, not float64: NormalizedChatResponse is consumed in-process by the FinOps ledger,
		// and a float64 there would make this adapter's output inconsistent with every other provider's.
		usage, _ := out["usage"].(map[string]any)
		if usage["prompt_tokens"] != 12 || usage["completion_tokens"] != 20 {
			t.Errorf("usage = %v, want 12/20 as ints", usage)
		}

		// OBS-007 and OBS-005: both come from the SDK's ResponseMeta and both have to survive the
		// mapping, because neither can be recovered later from anywhere else.
		if out["provider_request_id"] != "req-1" {
			t.Errorf("provider_request_id = %v, want req-1 — without it our ledger and the platform's have no key in common", out["provider_request_id"])
		}
		if out["served_by_instance"] != "qwen3-local-1" {
			t.Errorf("served_by_instance = %v, want qwen3-local-1", out["served_by_instance"])
		}
		if _, present := out["idempotent_replay_of"]; present {
			t.Error("idempotent_replay_of is present on a real generation; its absence is what means 'this really generated'")
		}
	})

	t.Run("a replay is marked, and only when the platform says so", func(t *testing.T) {
		srv := fakeGateway(t, chatResponse("hola"), map[string]string{
			"X-Request-ID":           "req-2",
			"Idempotent-Replay":      "true",
			"X-Idempotent-Replay-Of": "req-1",
		})
		adapter := prometheusinference.New("", srv.URL, "id", "secret", "inference:read model:test-model", "test-model")

		out, err := adapter.Decide(ctx, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if out["idempotent_replay_of"] != "req-1" {
			t.Errorf("idempotent_replay_of = %v, want req-1 — the id is the only part a caller can act on, and OBS-006 bills on it", out["idempotent_replay_of"])
		}
	})

	t.Run("an unmapped field is refused rather than forwarded in silence", func(t *testing.T) {
		// The gateway's request schema is an allowlist that discards what it does not recognise WITHOUT
		// saying so — Synaptum lost an afternoon to response_format doing exactly that. So the mapping
		// is explicit, and a request with no model or no messages fails here instead of travelling and
		// coming back wrong.
		srv := fakeGateway(t, chatResponse("hola"), nil)
		adapter := prometheusinference.New("", srv.URL, "id", "secret", "scope", "")

		if _, err := adapter.Decide(ctx, map[string]any{"messages": []any{}}); err == nil {
			t.Error("a call with no model and no default was accepted")
		}
		adapterWithModel := prometheusinference.New("", srv.URL, "id", "secret", "scope", "test-model")
		if _, err := adapterWithModel.Decide(ctx, map[string]any{}); err == nil {
			t.Error("a call with no messages was accepted")
		}
	})

	t.Run("the cost model and caching capability are still ours to declare", func(t *testing.T) {
		// Not the SDK's to answer: what a provider COSTS and whether it caches are routing and FinOps
		// facts about this deployment, and ADR-003's Budgeter reads the caching one.
		adapter := prometheusinference.New("", "http://unused.invalid", "id", "secret", "scope", "m")
		if got := adapter.CostModel(); got != "token_based" {
			t.Errorf("CostModel = %q — OBS-007 measured that this platform bills per token, not per GPU-second", got)
		}
		if got := adapter.CachingCapability(); got == "" {
			t.Error("CachingCapability is empty; the Budgeter reads it to decide whether keeping context is cheaper than summarising")
		}
	})
}
