package modelgateway

import (
	"strings"
	"testing"
)

// TestDeclaredModalityIsCheckedAgainstTheCatalog is MDL-013's acceptance test.
//
// It closes a limit MDL-011 assumed rather than hid: until now Aeon verified what the OPERATOR WROTE,
// never what the model IS. A bundle could declare an embeddings model as `text`, pass MDL-011 because
// text is chat-servable, and fail on the first real call — with an error from the provider, about a
// model the bundle had asserted was fine.
//
// Viable because the catalog is nearly free, measured on the real deployment rather than assumed:
// 2030 bytes, ~1ms, and NO AUTHENTICATION required. Two of those numbers differ from Axonium's
// (they measured 1020 bytes and 12ms cold) and neither of us is wrong — the catalog doubled when the
// platform added two modalities, which is exactly the kind of drift this check exists to notice.
//
// Axonium's own SDK has verify_modality OFF by default, for a reason that does not apply to us and is
// worth writing down: an SDK must not make requests its caller did not ask for. A gateway that resolves
// a profile into a concrete model IS the thing being asked, and it makes the request once at startup,
// not per call.
func TestDeclaredModalityIsCheckedAgainstTheCatalog(t *testing.T) {
	bundle := func(modality, model string) ModelPolicyBundleDoc {
		return ModelPolicyBundleDoc{Profiles: []ModelProfileDoc{{
			Profile: "reasoning-local",
			Candidates: []CandidateDoc{{
				Provider: "prometheus_inference", Model: model, Modality: modality,
				InferenceClass: "local", Priority: 0, CostModel: "token_based",
			}},
		}}}
	}
	catalog := []CatalogEntry{
		{Model: "qwen3-0.6b", Modality: "text"},
		{Model: "qwen3-embedding", Modality: "embedding"},
		{Model: "sst2-clf", Modality: "classification"},
	}

	t.Run("a truthful declaration passes", func(t *testing.T) {
		if got := VerifyModalitiesAgainstCatalog(bundle("text", "qwen3-0.6b"), "prometheus_inference", catalog); len(got) != 0 {
			t.Fatalf("contradictions = %v, want none", got)
		}
	})

	t.Run("an embeddings model declared as text is caught, which MDL-011 could not do", func(t *testing.T) {
		// The exact hole. `text` is chat-servable, so MDL-011 is satisfied; the catalog says otherwise.
		got := VerifyModalitiesAgainstCatalog(bundle("text", "qwen3-embedding"), "prometheus_inference", catalog)
		if len(got) != 1 {
			t.Fatalf("contradictions = %v, want exactly 1", got)
		}
		if got[0].Declared != "text" || got[0].Actual != "embedding" {
			t.Errorf("contradiction = %+v, want declared text / actual embedding", got[0])
		}
		if !strings.Contains(got[0].Error(), "catalog says") {
			t.Errorf("message = %q, should say what the catalog says instead", got[0].Error())
		}
	})

	t.Run("a model absent from the catalog is a DIFFERENT contradiction, not the same one", func(t *testing.T) {
		// Worth distinguishing: a wrong modality is a mistake in the bundle, an absent model is a bundle
		// naming something that does not exist — or a model retired underneath a deployment that was
		// working yesterday. The second needs a different conversation than the first.
		got := VerifyModalitiesAgainstCatalog(bundle("text", "model-that-was-retired"), "prometheus_inference", catalog)
		if len(got) != 1 {
			t.Fatalf("contradictions = %v, want exactly 1", got)
		}
		if got[0].Actual != "" {
			t.Errorf("Actual = %q, want empty to mean 'not in the catalog at all'", got[0].Actual)
		}
		if !strings.Contains(got[0].Error(), "does not list at all") {
			t.Errorf("message = %q, should distinguish absence from disagreement", got[0].Error())
		}
	})

	t.Run("another provider's candidates are left alone", func(t *testing.T) {
		// The catalog belongs to one provider. Judging an Anthropic candidate against Prometheus's
		// catalog would report every one of them as absent — a check that fires on correct configuration
		// is worse than no check, because it teaches people to ignore it.
		doc := ModelPolicyBundleDoc{Profiles: []ModelProfileDoc{{
			Profile: "reasoning-high",
			Candidates: []CandidateDoc{
				{Provider: "anthropic", Model: "claude-opus-5", Modality: "text", CostModel: "token_based"},
				{Provider: "prometheus_inference", Model: "qwen3-0.6b", Modality: "text", CostModel: "token_based"},
			},
		}}}
		if got := VerifyModalitiesAgainstCatalog(doc, "prometheus_inference", catalog); len(got) != 0 {
			t.Fatalf("contradictions = %v, want none — the Anthropic candidate is not this catalog's business", got)
		}
	})

	t.Run("an empty catalog reports everything, which is why the CALLER decides", func(t *testing.T) {
		// This is the case with teeth, and the reason fetching lives outside this function.
		//
		// A catalog that could not be read arrives here as an empty slice and every declaration then
		// looks absent. If this function also did the fetching it would be free to treat that as "all
		// wrong" and refuse to serve — so a transient platform blip would take the gateway down, which is
		// worse than the problem MDL-013 solves. Separating them forces the caller to distinguish
		// "verified and wrong" from "not verified", and that is a three-state rule, not a two-state one.
		got := VerifyModalitiesAgainstCatalog(bundle("text", "qwen3-0.6b"), "prometheus_inference", nil)
		if len(got) != 1 || got[0].Actual != "" {
			t.Fatalf("contradictions = %v, want the declaration reported as absent", got)
		}
	})
}
