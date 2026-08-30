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

type fakeDecideProvider struct {
	fail bool
}

func (f *fakeDecideProvider) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	if f.fail {
		return nil, errors.New("simulated provider failure")
	}
	return map[string]any{"model": renderedContext["model"], "choices": []any{
		map[string]any{"message": map[string]any{"role": "assistant", "content": "hi"}},
	}}, nil
}
func (f *fakeDecideProvider) CachingCapability() string { return "none" }
func (f *fakeDecideProvider) CostModel() string         { return "token_based" }

func newModelGatewayTestServer(t *testing.T, fail bool) *httptest.Server {
	t.Helper()
	gw := modelgateway.New()
	gw.RegisterProvider("fake", &fakeDecideProvider{fail: fail})
	mux := http.NewServeMux()
	(&ModelGatewayHandlers{Gateway: gw}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postDecide(t *testing.T, srv *httptest.Server, body decideRequest) (status int, parsed map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/decide", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST /decide: %v", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, parsed
}

// TestModelGatewayDecideHandler proves the HTTP surface over modelgateway.Gateway.Decide (MDL-001)
// round-trips a real candidate/rendered_context request into the same normalized shape the Go
// callers get, and reports routing failure as 502 rather than 400/500 — this is the seam DR-001's
// Research Planner (and every later F2 feature) calls from the Python worker.
func TestModelGatewayDecideHandler(t *testing.T) {
	t.Run("routes to the registered provider and returns its normalized output", func(t *testing.T) {
		srv := newModelGatewayTestServer(t, false)
		status, body := postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: "fake", Model: "test-model", Priority: 0}},
			RenderedContext: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", status, body)
		}
		if body["provider_used"] != "fake" {
			t.Fatalf("provider_used = %v, want fake", body["provider_used"])
		}
		output, ok := body["output"].(map[string]any)
		if !ok || output["model"] != "test-model" {
			t.Fatalf("expected output.model = test-model, got %v", body["output"])
		}
	})

	t.Run("empty candidates is a 400, not a panic", func(t *testing.T) {
		srv := newModelGatewayTestServer(t, false)
		status, _ := postDecide(t, srv, decideRequest{RenderedContext: map[string]any{}})
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", status)
		}
	})

	t.Run("every candidate failing is a 502, not a 500", func(t *testing.T) {
		srv := newModelGatewayTestServer(t, true)
		status, body := postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: "fake", Model: "test-model", Priority: 0}},
			RenderedContext: map[string]any{},
		})
		if status != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body = %v", status, body)
		}
		if _, hasError := body["error"]; !hasError {
			t.Fatalf("expected an error field, got %v", body)
		}
	})
}
