package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
)

func newInferenceClassTestServer(t *testing.T, profiles []modelgateway.ModelProfileDoc) (*httptest.Server, *countingProvider) {
	t.Helper()
	provider := &countingProvider{}
	gw := modelgateway.New()
	// Both are registered and both work. Nothing in this test fails for a technical reason: every
	// denial below is policy, which is the only way to tell the two apart.
	gw.RegisterProvider("prometheus_inference", provider)
	gw.RegisterProvider("openai_compatible", provider)

	mux := http.NewServeMux()
	(&OpenAICompatibleHandlers{Gateway: gw, Bundle: modelgateway.ModelPolicyBundleDoc{Profiles: profiles}}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, provider
}

func chatFor(profile string) map[string]any {
	return map[string]any{"model": profile, "messages": []any{map[string]any{"role": "user", "content": "hola"}}}
}

// TestLocalInferenceOutsidePrometheusIsDenied is MDL-008's acceptance test.
//
// The platform rule, set by the project owner, is that all local inference resolves in Prometheus
// and the only door is the Axonium SDK. The shape of the enforcement was fixed in the tripartite
// agreement, and it is the part that matters: **default-deny with a nominal exception**, never
// default-allow with a deny rule layered on top. The difference is not stylistic. A deny rule only
// ever fires on candidates someone remembered to annotate, and the candidates nobody annotated are
// exactly where mistakes live — so "we enforce this as policy" would quietly degrade into "we
// intended to enforce it".
func TestLocalInferenceOutsidePrometheusIsDenied(t *testing.T) {
	local := func(provider, model string) modelgateway.CandidateDoc {
		return modelgateway.CandidateDoc{
			Provider: provider, Model: model,
			Modality: modelgateway.ModalityText, InferenceClass: modelgateway.InferenceClassLocal,
		}
	}

	t.Run("a local candidate on another provider is denied", func(t *testing.T) {
		srv, provider := newInferenceClassTestServer(t, []modelgateway.ModelProfileDoc{
			{Profile: "local-elsewhere", Candidates: []modelgateway.CandidateDoc{local("openai_compatible", "llama-local")}},
		})

		status, parsed := postChatCompletion(t, srv, chatFor("local-elsewhere"))
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
		if provider.calls.Load() != 0 {
			t.Fatalf("the provider was called %d time(s) — a denial that still runs the call is not a denial", provider.calls.Load())
		}
		errObj, _ := parsed["error"].(map[string]any)
		if errObj["type"] != "aeon_policy_denied" {
			t.Errorf("error type = %v, want aeon_policy_denied", errObj["type"])
		}
		// The message has to name what the operator can act on. "Access denied" would send someone
		// hunting through provider credentials for a decision that was made in a YAML file.
		if msg, _ := errObj["message"].(string); msg == "" || !strings.Contains(msg, "no local_inference exception is declared") {
			t.Errorf("message = %q, want it to say the exception is missing", msg)
		}
	})

	t.Run("an undeclared inference class is denied, not assumed", func(t *testing.T) {
		// This is the default-deny half, and the reason the field is required. If unspecified meant
		// "cloud", every candidate anyone forgot to annotate would route freely — and forgetting is
		// the failure mode this exists for.
		srv, provider := newInferenceClassTestServer(t, []modelgateway.ModelProfileDoc{
			{Profile: "silent", Candidates: []modelgateway.CandidateDoc{
				{Provider: "openai_compatible", Model: "who-knows", Modality: modelgateway.ModalityText},
			}},
		})

		status, parsed := postChatCompletion(t, srv, chatFor("silent"))
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 for a candidate that does not say where it runs", status)
		}
		if provider.calls.Load() != 0 {
			t.Fatalf("the provider was called %d time(s)", provider.calls.Load())
		}
		errObj, _ := parsed["error"].(map[string]any)
		if msg, _ := errObj["message"].(string); !strings.Contains(msg, "does not declare inference_class") {
			t.Errorf("message = %q, want it to name the missing declaration", msg)
		}
	})

	t.Run("prometheus needs no exception — it is the door", func(t *testing.T) {
		srv, provider := newInferenceClassTestServer(t, []modelgateway.ModelProfileDoc{
			{Profile: "local-prometheus", Candidates: []modelgateway.CandidateDoc{local("prometheus_inference", "local-default")}},
		})

		if status, _ := postChatCompletion(t, srv, chatFor("local-prometheus")); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if provider.calls.Load() != 1 {
			t.Fatalf("provider calls = %d, want 1", provider.calls.Load())
		}
	})

	t.Run("a named exception for a declared environment allows it", func(t *testing.T) {
		// The exception is the whole reason default-deny is workable: developing without a GPU is a
		// real need, and a rule with no legitimate way through gets disabled rather than followed.
		srv, provider := newInferenceClassTestServer(t, []modelgateway.ModelProfileDoc{
			{
				Profile:        "local-dev",
				Candidates:     []modelgateway.CandidateDoc{local("openai_compatible", "llama-local")},
				LocalInference: &modelgateway.LocalInferenceException{Environment: "dev-laptop", AllowedProviders: []string{"openai_compatible"}},
			},
		})

		if status, _ := postChatCompletion(t, srv, chatFor("local-dev")); status != http.StatusOK {
			t.Fatalf("status = %d, want 200 with the exception in place", status)
		}
		if provider.calls.Load() != 1 {
			t.Fatalf("provider calls = %d, want 1", provider.calls.Load())
		}
	})

	t.Run("an exception is nominal: it covers the provider it names and no other", func(t *testing.T) {
		srv, provider := newInferenceClassTestServer(t, []modelgateway.ModelProfileDoc{
			{
				Profile:        "local-dev-other",
				Candidates:     []modelgateway.CandidateDoc{local("openai_compatible", "llama-local")},
				LocalInference: &modelgateway.LocalInferenceException{Environment: "dev-laptop", AllowedProviders: []string{"some_other_provider"}},
			},
		})

		status, parsed := postChatCompletion(t, srv, chatFor("local-dev-other"))
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 — an exception that covers everything is not an exception", status)
		}
		if provider.calls.Load() != 0 {
			t.Fatalf("the provider was called %d time(s)", provider.calls.Load())
		}
		// The denial names the exception that exists and did not cover this: far more useful during
		// an incident than reporting its absence.
		errObj, _ := parsed["error"].(map[string]any)
		if msg, _ := errObj["message"].(string); !strings.Contains(msg, "dev-laptop") {
			t.Errorf("message = %q, want it to name the environment whose exception did not cover this", msg)
		}
	})

	t.Run("an exception on one profile does not reach another in the same bundle", func(t *testing.T) {
		srv, provider := newInferenceClassTestServer(t, []modelgateway.ModelProfileDoc{
			{
				Profile:        "dev",
				Candidates:     []modelgateway.CandidateDoc{local("openai_compatible", "llama-local")},
				LocalInference: &modelgateway.LocalInferenceException{Environment: "dev-laptop", AllowedProviders: []string{"openai_compatible"}},
			},
			{Profile: "prod", Candidates: []modelgateway.CandidateDoc{local("openai_compatible", "llama-local")}},
		})

		if status, _ := postChatCompletion(t, srv, chatFor("prod")); status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 — the exception is scoped to the profile that declares it", status)
		}
		if provider.calls.Load() != 0 {
			t.Fatalf("the provider was called %d time(s)", provider.calls.Load())
		}
	})
}
