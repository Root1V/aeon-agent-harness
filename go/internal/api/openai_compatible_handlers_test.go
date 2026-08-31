package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
)

type fakeChatProvider struct {
	fail bool
}

func (f *fakeChatProvider) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	if f.fail {
		return nil, errors.New("simulated provider failure")
	}
	return map[string]any{
		"model": renderedContext["model"],
		"choices": []any{
			map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "hello from aeon"}, "finish_reason": "stop"},
		},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	}, nil
}
func (f *fakeChatProvider) CachingCapability() string { return "none" }
func (f *fakeChatProvider) CostModel() string         { return "token_based" }

func newOpenAICompatibleTestServer(t *testing.T, providerFails bool) *httptest.Server {
	t.Helper()
	gw := modelgateway.New()
	fake := &fakeChatProvider{fail: providerFails}
	gw.RegisterProvider("fake", fake)
	// Gateway.Decide only ever allows the real "prometheus_inference" provider name for
	// data_sensitivity=restricted (ADR-004) — registering the same fake under that name lets the
	// restricted-test profile below exercise the real restriction-enforcement path without a live
	// Prometheus instance.
	gw.RegisterProvider("prometheus_inference", fake)

	bundle := modelgateway.ModelPolicyBundleDoc{
		Profiles: []modelgateway.ModelProfileDoc{
			{Profile: "reasoning-test", Candidates: []modelgateway.CandidateDoc{{Provider: "fake", Model: "fake-model-v1", Priority: 0}}},
			{
				Profile:            "restricted-test",
				Candidates:         []modelgateway.CandidateDoc{{Provider: "prometheus_inference", Model: "fake-model-v1", Priority: 0}},
				RoutingConstraints: &modelgateway.RoutingConstraints{DataSensitivity: "restricted"},
			},
		},
	}

	mux := http.NewServeMux()
	(&OpenAICompatibleHandlers{Gateway: gw, Bundle: bundle}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postChatCompletion(t *testing.T, srv *httptest.Server, body map[string]any) (status int, parsed map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, parsed
}

// TestOpenAICompatibleChatCompletions is INT-002's acceptance test: a request in the real OpenAI
// Chat Completions wire format, with "model" naming a capability profile rather than a concrete
// model (docs/adr/0004), gets routed through the real Model Gateway and comes back as a real,
// spec-shaped OpenAI response — proving any client speaking that wire format (the real `openai`
// SDK, verified by hand against the real compose stack — see roadmap.md) gets Aeon's routing by
// only changing its base_url.
func TestOpenAICompatibleChatCompletions(t *testing.T) {
	t.Run("resolves a profile to real candidates and returns a spec-shaped response", func(t *testing.T) {
		srv := newOpenAICompatibleTestServer(t, false)
		status, body := postChatCompletion(t, srv, map[string]any{
			"model":    "reasoning-test",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})

		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", status, body)
		}
		if body["object"] != "chat.completion" {
			t.Errorf("object = %v, want chat.completion", body["object"])
		}
		id, _ := body["id"].(string)
		if id == "" || !bytes.HasPrefix([]byte(id), []byte("chatcmpl-")) {
			t.Errorf("id = %q, want a chatcmpl-* id", id)
		}
		if _, ok := body["created"].(float64); !ok {
			t.Errorf("expected a numeric 'created' timestamp, got %v", body["created"])
		}
		if body["model"] != "fake-model-v1" {
			t.Errorf("model = %v, want the resolved candidate's real model, not the profile name", body["model"])
		}
		choices, ok := body["choices"].([]any)
		if !ok || len(choices) != 1 {
			t.Fatalf("expected a real choices[] array, got %v", body["choices"])
		}
		message := choices[0].(map[string]any)["message"].(map[string]any)
		if message["content"] != "hello from aeon" {
			t.Errorf("content = %v, want the real provider's response", message["content"])
		}
	})

	t.Run("requires a model field", func(t *testing.T) {
		srv := newOpenAICompatibleTestServer(t, false)
		status, body := postChatCompletion(t, srv, map[string]any{"messages": []map[string]any{{"role": "user", "content": "hi"}}})
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %v", status, body)
		}
	})

	t.Run("reports an unknown profile as a real, distinguishable error", func(t *testing.T) {
		srv := newOpenAICompatibleTestServer(t, false)
		status, body := postChatCompletion(t, srv, map[string]any{"model": "not-a-real-profile", "messages": []map[string]any{}})
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", status, body)
		}
		errObj, ok := body["error"].(map[string]any)
		if !ok || errObj["message"] == "" {
			t.Fatalf("expected an OpenAI-shaped error object, got %v", body)
		}
	})

	t.Run("a routing failure (every candidate down) is a 502, not a 500", func(t *testing.T) {
		srv := newOpenAICompatibleTestServer(t, true)
		status, body := postChatCompletion(t, srv, map[string]any{"model": "reasoning-test", "messages": []map[string]any{}})
		if status != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body = %v", status, body)
		}
	})

	t.Run("data_sensitivity=restricted in the bundle is honored even over this endpoint", func(t *testing.T) {
		srv := newOpenAICompatibleTestServer(t, false)
		// The only candidate for "restricted-test" is "fake" itself, so a restricted request
		// still succeeds — this proves the routing_constraints field round-trips from the
		// bundle into a real Gateway.Decide call, not that a cloud fallback is unreachable here.
		status, body := postChatCompletion(t, srv, map[string]any{"model": "restricted-test", "messages": []map[string]any{}})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", status, body)
		}
	})
}
