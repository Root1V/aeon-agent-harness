package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
)

// TestQualityAwareRoutingChangesWithADegradedScore is MDL-002's acceptance test: before any score
// is reported, the real Model Gateway routes to the higher-priority candidate as usual. After a
// real, low score is reported for that exact candidate over real HTTP, routing genuinely changes —
// the very next /decide call skips it entirely and falls through to the next candidate — all
// against a real Postgres-backed QualityScoreStore, not a fixture rigged in memory.
func TestQualityAwareRoutingChangesWithADegradedScore(t *testing.T) {
	s := newAPITestStore(t)
	scores := s.QualityScores(0.9)
	model := "quality-test-model-" + randSuffix(t)

	gw := modelgateway.New()
	gw.RegisterProvider("primary-provider", &fakeDecideProvider{})
	gw.RegisterProvider("fallback-provider", &fakeDecideProvider{})
	gw.Quality = scores

	mux := http.NewServeMux()
	(&ModelGatewayHandlers{Gateway: gw}).Register(mux)
	(&QualityScoreHandlers{Scores: scores}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	decideBody := decideRequest{
		Candidates: []decideCandidate{
			{Provider: "primary-provider", Model: model, Priority: 0},
			{Provider: "fallback-provider", Model: "m2", Priority: 1},
		},
		RenderedContext: map[string]any{"messages": []any{}},
	}

	t.Run("before any score is reported, the higher-priority candidate is used", func(t *testing.T) {
		status, body := postDecide(t, srv, decideBody)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", status, body)
		}
		if body["provider_used"] != "primary-provider" {
			t.Fatalf("provider_used = %v, want primary-provider", body["provider_used"])
		}
	})

	reportBody, _ := json.Marshal(reportQualityScoreRequest{Provider: "primary-provider", Model: model, Suite: "provider_conformance", Score: 0.4})
	resp, err := http.Post(srv.URL+"/quality-scores", "application/json", bytes.NewReader(reportBody))
	if err != nil {
		t.Fatalf("POST /quality-scores: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /quality-scores: status = %d", resp.StatusCode)
	}

	t.Run("after a degraded score is reported, routing skips it for the next candidate", func(t *testing.T) {
		status, body := postDecide(t, srv, decideBody)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", status, body)
		}
		if body["provider_used"] != "fallback-provider" {
			t.Fatalf("provider_used = %v, want fallback-provider — the degraded primary-provider should have been skipped", body["provider_used"])
		}
		attempts, _ := body["attempts"].([]any)
		if len(attempts) != 2 {
			t.Fatalf("expected 2 attempts (the skip recorded), got %v", attempts)
		}
	})

	t.Run("GET /quality-scores lists the real reported score", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/quality-scores")
		if err != nil {
			t.Fatalf("GET /quality-scores: %v", err)
		}
		defer resp.Body.Close()
		var parsed map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		found := false
		for _, s := range parsed["scores"].([]any) {
			row, _ := s.(map[string]any)
			if row["provider"] == "primary-provider" && row["model"] == model {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the reported score to appear in the list, got %v", parsed["scores"])
		}
	})
}
