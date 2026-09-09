package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
)

// countingProvider records whether it was reached at all. That counter is the whole point of this
// test: the fault MDL-011 prevents is not "a bad answer", it is a *billable* one, so proving the
// request was refused is only half of it — the other half is proving nothing was called.
type countingProvider struct {
	calls atomic.Int64
}

func (c *countingProvider) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	c.calls.Add(1)
	return map[string]any{
		"model":   renderedContext["model"],
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": ""}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 900, "completion_tokens": 1},
	}, nil
}
func (c *countingProvider) CachingCapability() string { return "none" }
func (c *countingProvider) CostModel() string         { return "token_based" }

func newModalityTestServer(t *testing.T) (*httptest.Server, *countingProvider) {
	t.Helper()
	provider := &countingProvider{}
	gw := modelgateway.New()
	gw.RegisterProvider("prometheus_inference", provider)

	bundle := modelgateway.ModelPolicyBundleDoc{
		Profiles: []modelgateway.ModelProfileDoc{
			{Profile: "chat-ok", Candidates: []modelgateway.CandidateDoc{
				{Provider: "prometheus_inference", Model: "chat-model", Modality: modelgateway.ModalityChat, Priority: 0},
			}},
			// The real misconfiguration: an embeddings model sitting in a chat profile.
			{Profile: "chat-with-embeddings-model", Candidates: []modelgateway.CandidateDoc{
				{Provider: "prometheus_inference", Model: "embed-model", Modality: "embedding", Priority: 0},
			}},
			// The silent version of the same mistake: the bundle never says what the model is.
			{Profile: "chat-undeclared", Candidates: []modelgateway.CandidateDoc{
				{Provider: "prometheus_inference", Model: "who-knows", Priority: 0},
			}},
			// A misclassified candidate must not be rescued by a healthy sibling: the bundle is
			// wrong, and routing around the error would hide it.
			{Profile: "chat-mixed", Candidates: []modelgateway.CandidateDoc{
				{Provider: "prometheus_inference", Model: "chat-model", Modality: modelgateway.ModalityChat, Priority: 0},
				{Provider: "prometheus_inference", Model: "embed-model", Modality: "embedding", Priority: 1},
			}},
		},
	}

	mux := http.NewServeMux()
	(&OpenAICompatibleHandlers{Gateway: gw, Bundle: bundle}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, provider
}

// TestEmbeddingModelRoutedAsChatIsRejected is MDL-011's acceptance test. It comes from a real
// finding by the Axonium team against the live Prometheus deployment: calling
// /v1/chat/completions with an embeddings model does not fail — it answers 200 with degenerate
// output, and that output is billed. For this gateway that is a routing fault of the worst kind,
// because the fallback cascade only advances when a candidate *fails*. A degenerate 200 is never
// retried against the next candidate: it is accepted, returned, and written to the FinOps ledger as
// a legitimate call. No exception, no retry, and an invoice.
func TestEmbeddingModelRoutedAsChatIsRejected(t *testing.T) {
	chatBody := func(profile string) map[string]any {
		return map[string]any{
			"model":    profile,
			"messages": []any{map[string]any{"role": "user", "content": "hola"}},
		}
	}

	t.Run("an embeddings candidate is refused before any provider is called", func(t *testing.T) {
		srv, provider := newModalityTestServer(t)

		status, parsed := postChatCompletion(t, srv, chatBody("chat-with-embeddings-model"))
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
		if provider.calls.Load() != 0 {
			t.Fatalf("the provider was called %d time(s) — the point of checking at resolution is that nothing is billed", provider.calls.Load())
		}
		errObj, _ := parsed["error"].(map[string]any)
		if errObj["type"] != "aeon_policy_denied" {
			t.Errorf("error type = %v, want aeon_policy_denied — a caller cannot act on a denial it cannot tell apart from a typo", errObj["type"])
		}
	})

	t.Run("a candidate that declares nothing is refused too", func(t *testing.T) {
		// Silence is not a permission. The failure being prevented is a bundle that never says what
		// a model is, so treating "unspecified" as "chat" would leave the original bug intact.
		srv, provider := newModalityTestServer(t)

		status, _ := postChatCompletion(t, srv, chatBody("chat-undeclared"))
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 for a candidate with no declared modality", status)
		}
		if provider.calls.Load() != 0 {
			t.Fatalf("the provider was called %d time(s)", provider.calls.Load())
		}
	})

	t.Run("one bad candidate fails the profile, it is not routed around", func(t *testing.T) {
		srv, provider := newModalityTestServer(t)

		status, _ := postChatCompletion(t, srv, chatBody("chat-mixed"))
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 — skipping the misclassified candidate would quietly ignore what the operator wrote", status)
		}
		if provider.calls.Load() != 0 {
			t.Fatalf("the healthy sibling served the call (%d), hiding the configuration error", provider.calls.Load())
		}
	})

	t.Run("a denial stays distinguishable from an unknown profile", func(t *testing.T) {
		// Both used to be 404 invalid_request_error, which is the shape of a client typo. If a
		// denial looks like a typo, nobody investigates it.
		srv, _ := newModalityTestServer(t)

		status, parsed := postChatCompletion(t, srv, chatBody("no-such-profile"))
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 for an unknown profile", status)
		}
		errObj, _ := parsed["error"].(map[string]any)
		if errObj["type"] != "invalid_request_error" {
			t.Errorf("error type = %v, want invalid_request_error", errObj["type"])
		}
	})

	t.Run("a correctly declared chat profile still routes", func(t *testing.T) {
		srv, provider := newModalityTestServer(t)

		status, _ := postChatCompletion(t, srv, chatBody("chat-ok"))
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if provider.calls.Load() != 1 {
			t.Fatalf("provider calls = %d, want 1", provider.calls.Load())
		}
	})
}
