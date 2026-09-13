package modelgateway

import (
	"errors"
	"fmt"

	"github.com/aeon-ai/aeon/go/internal/finops"
)

// ModelPolicyBundleDoc mirrors proto/manifests/model_policy_bundle.schema.json — the config-as-code
// (FND-003) binding of capability profiles to concrete provider/model candidates. Loaded from a
// real YAML/JSON file, same pattern as go/internal/policy.PolicyBundleDoc, never hardcoded.
type ModelPolicyBundleDoc struct {
	APIVersion string            `json:"apiVersion" yaml:"apiVersion"`
	Kind       string            `json:"kind" yaml:"kind"`
	Profiles   []ModelProfileDoc `json:"profiles" yaml:"profiles"`
}

// ModelProfileDoc is one profile -> candidates[] entry, mirroring proto/schemas/model_profile.schema.json.
type ModelProfileDoc struct {
	Profile    string         `json:"profile" yaml:"profile"`
	Candidates []CandidateDoc `json:"candidates" yaml:"candidates"`
	// LocalInference is MDL-008's nominal exception, and it is scoped to the profile rather than to
	// the bundle on purpose: an exception granted to the profile that needs it cannot silently
	// widen to the others sitting in the same file.
	LocalInference     *LocalInferenceException `json:"local_inference,omitempty" yaml:"local_inference,omitempty"`
	RoutingConstraints *RoutingConstraints      `json:"routing_constraints,omitempty" yaml:"routing_constraints,omitempty"`
}

// LocalInferenceException names which providers may serve local inference, and for which declared
// environment. There is no implicit one: a candidate declaring inference_class=local is denied
// unless a profile lists its provider here.
//
// It lives in the bundle — in Git, under review — rather than in an environment variable, because
// an exception nobody can diff is not an exception, it is a hole. Environment is not decoration
// either: it is what makes "why was this allowed" answerable from the file itself, months later,
// by someone who was not in the conversation.
type LocalInferenceException struct {
	Environment      string   `json:"environment" yaml:"environment"`
	AllowedProviders []string `json:"allowed_providers" yaml:"allowed_providers"`
}

// CandidateDoc is one candidate entry within a profile — the subset of model_profile.schema.json's
// candidate fields the router actually needs, plus the pricing fields OBS-003's FinOps ledger
// reads (CostModel/CostPerMillion*Tokens); tool_calling/caching_capability/max_context_tokens are
// documented there but not yet consumed here (see roadmap.md MDL-002).
type CandidateDoc struct {
	Provider string `json:"provider" yaml:"provider"`
	Model    string `json:"model" yaml:"model"`
	// Modality is what the model actually is (MDL-011). Required: silence is not a permission,
	// because the failure this prevents is precisely a bundle that never says what a model is.
	Modality string `json:"modality" yaml:"modality"`
	// InferenceClass is where the model actually runs (MDL-008), "local" or "cloud". Required, and
	// undeclared is denied rather than assumed — see ErrInferenceClassUndeclared.
	InferenceClass             string  `json:"inference_class" yaml:"inference_class"`
	Priority                   int     `json:"priority" yaml:"priority"`
	CostModel                  string  `json:"cost_model,omitempty" yaml:"cost_model,omitempty"`
	CostPerMillionInputTokens  float64 `json:"cost_per_million_input_tokens,omitempty" yaml:"cost_per_million_input_tokens,omitempty"`
	CostPerMillionOutputTokens float64 `json:"cost_per_million_output_tokens,omitempty" yaml:"cost_per_million_output_tokens,omitempty"`
}

// RoutingConstraints mirrors model_profile.schema.json's routing_constraints — today just
// data_sensitivity, which Gateway.Decide already enforces (ADR-004: restricted data never leaves
// the local network).
type RoutingConstraints struct {
	DataSensitivity string `json:"data_sensitivity,omitempty" yaml:"data_sensitivity,omitempty"`
	// InNetworkProviders names the providers that satisfy data_sensitivity=restricted for this
	// profile — the ones whose traffic never leaves the network. The harness has no opinion about
	// which those are: that is a fact about a deployment's topology, and hardcoding one platform's
	// name here is what MDL-017 removed.
	//
	// Deliberately NOT the same list as local_inference.providers, for the reason MDL-008 already
	// recorded about these two rules: a self-hosted model in a private VPC is in-network without
	// being local inference, and folding them together would let one declaration quietly widen the
	// other.
	InNetworkProviders []string `json:"in_network_providers,omitempty" yaml:"in_network_providers,omitempty"`
}

// ErrProfileNotFound is returned when a requested profile has no entry in the bundle.
var ErrProfileNotFound = errors.New("modelgateway: profile not found in ModelPolicyBundle")

// The modalities a candidate can declare. These are the Prometheus catalog's own values
// (contratos/gateway-prometheus/modalidades.md), adopted rather than translated — a private
// vocabulary would need a mapping, and the mapping is the part that goes wrong.
const (
	ModalityText      = "text"
	ModalityVision    = "vision"
	ModalityEmbedding = "embedding"
	ModalityImage     = "image"
)

// chatServableModalities is the correspondence that is NOT one to one, and the reason this is a set
// rather than an equality check: text and vision are BOTH served on /v1/chat/completions. A vision
// model is called exactly like a text one; the only difference is that it accepts image_url content
// parts. The first version of this check compared against a single "chat" value and would have
// rejected a perfectly valid vision model — failing the whole profile, which is the consequence
// deliberately chosen for a *bad* candidate, applied to a good one.
var chatServableModalities = map[string]bool{ModalityText: true, ModalityVision: true}

// ErrCandidateModalityMismatch is MDL-011: a candidate in a chat profile is not a chat model, or
// does not say what it is.
//
// It exists as its own sentinel, and the resolution fails rather than skipping the candidate, for
// a reason found by the Axonium team against the real Prometheus deployment: calling
// /v1/chat/completions with an embeddings model does not fail. It answers 200 with degenerate
// output that is billed. For a gateway that is the worst kind of routing fault — the fallback
// cascade only advances when a candidate *fails*, so a degenerate 200 is never retried against the
// next candidate. It is accepted, returned, and recorded in the FinOps ledger as a legitimate call.
// No exception, no retry, and an invoice.
//
// Skipping the bad candidate silently would be the same class of mistake one level up: quietly
// ignoring what the operator wrote. A misclassified candidate is a configuration error, so it fails
// where configuration errors belong — at resolution, before any call.
var ErrCandidateModalityMismatch = errors.New("modelgateway: candidate modality is not usable for a chat profile")

// InferenceClassLocal and InferenceClassCloud are the two declared places a model can run (MDL-008).
const (
	InferenceClassLocal = "local"
	InferenceClassCloud = "cloud"
)

// ErrInferenceClassUndeclared is MDL-008's default-deny: a candidate that does not say where it
// runs is denied, not assumed to be safe.
//
// The direction matters and was fixed in the tripartite agreement. If this were default-allow with
// a deny rule layered on top, "we enforce the platform rule as policy" would quietly degrade into
// "we intended to enforce it" — because the rule would only ever fire on the candidates someone
// remembered to annotate, and the ones nobody annotated are precisely where mistakes live.
var ErrInferenceClassUndeclared = errors.New("modelgateway: candidate does not declare inference_class, and undeclared is denied")

// ErrLocalInferenceProviderDenied is MDL-008's enforcement: a candidate declares it runs locally,
// but its provider is neither Prometheus nor named as an exception for a declared environment.
var ErrLocalInferenceProviderDenied = errors.New("modelgateway: local inference is only served by a provider the profile declares for it")

// ResolveProfile returns the candidates (in whatever order the bundle declares them — Decide sorts
// by Priority itself) and routing data_sensitivity, if any, for a named capability profile. A
// caller never names a concrete model — only a profile (docs/adr/0004) — so an unresolvable
// profile is a real, reportable error, not a silent empty candidate list.
func (doc ModelPolicyBundleDoc) ResolveProfile(profile string) ([]Candidate, string, error) {
	for _, p := range doc.Profiles {
		if p.Profile != profile {
			continue
		}
		inNetwork := map[string]bool{}
		if p.RoutingConstraints != nil {
			for _, provider := range p.RoutingConstraints.InNetworkProviders {
				inNetwork[provider] = true
			}
		}
		candidates := make([]Candidate, len(p.Candidates))
		for i, c := range p.Candidates {
			if err := checkInferenceClass(profile, c, p.LocalInference); err != nil {
				return nil, "", err
			}
			// An unknown value is denied rather than guessed. The catalog will grow (audio, rerank),
			// and a new value treated as chat produces a billable call with degenerate output —
			// exactly the failure this check exists to prevent.
			if !chatServableModalities[c.Modality] {
				declared := c.Modality
				if declared == "" {
					declared = "nothing"
				}
				return nil, "", fmt.Errorf("%w: profile %q candidate %s/%s declares %s, and a chat profile serves only %s or %s",
					ErrCandidateModalityMismatch, profile, c.Provider, c.Model, declared, ModalityText, ModalityVision)
			}
			candidates[i] = Candidate{Provider: c.Provider, Model: c.Model, Priority: c.Priority, InNetwork: inNetwork[c.Provider]}
		}
		dataSensitivity := ""
		if p.RoutingConstraints != nil {
			dataSensitivity = p.RoutingConstraints.DataSensitivity
		}
		return candidates, dataSensitivity, nil
	}
	return nil, "", fmt.Errorf("%w: %q", ErrProfileNotFound, profile)
}

// PricingRates extracts every candidate's pricing fields (across every profile) as a real
// finops.Rate slice — the config-as-code source OBS-003's PricingTable is built from. A candidate
// with no cost_model set is skipped (pricing wasn't configured for it, not priced at $0); the same
// (provider, model) appearing in multiple profiles is deduplicated implicitly by
// finops.NewPricingTable (last one wins, and in practice every profile referencing the same
// concrete model should declare the same real-world price anyway).
func (doc ModelPolicyBundleDoc) PricingRates() []finops.Rate {
	var rates []finops.Rate
	for _, p := range doc.Profiles {
		for _, c := range p.Candidates {
			if c.CostModel == "" {
				continue
			}
			rates = append(rates, finops.Rate{
				Provider:            c.Provider,
				Model:               c.Model,
				CostModel:           c.CostModel,
				InputPerMillionUSD:  c.CostPerMillionInputTokens,
				OutputPerMillionUSD: c.CostPerMillionOutputTokens,
			})
		}
	}
	return rates
}

// checkInferenceClass applies MDL-008 to one candidate.
//
// Note what it deliberately does NOT do: merge with the data_sensitivity=restricted rule, which
// filters by provider name. The two look similar and are not the same rule. Restricted is about a
// network boundary — sensitive data must never leave — and it must keep meaning "prometheus_inference
// and nothing else", including in an environment where an exception permits some other provider to
// serve local inference. Folding restricted into "any local candidate" would hand restricted data to
// whatever that exception named, which is the opposite of tightening.
func checkInferenceClass(profile string, c CandidateDoc, exception *LocalInferenceException) error {
	switch c.InferenceClass {
	case InferenceClassCloud:
		return nil
	case InferenceClassLocal:
		// No provider is allowed here by birthright. A standing rule like "all local inference
		// resolves in platform X" is true of a deployment, and belongs in that deployment's bundle —
		// a harness that ships it as a constant works for exactly one organisation (MDL-017).
		if exception != nil && exception.Environment != "" {
			for _, allowed := range exception.AllowedProviders {
				if allowed == c.Provider {
					return nil
				}
			}
		}
		return fmt.Errorf("%w: profile %q candidate %s/%s%s",
			ErrLocalInferenceProviderDenied, profile, c.Provider, c.Model, describeException(exception))
	default:
		return fmt.Errorf("%w: profile %q candidate %s/%s", ErrInferenceClassUndeclared, profile, c.Provider, c.Model)
	}
}

// describeException makes a denial legible to whoever reads it in an incident: the exception that
// exists and did not cover this candidate is far more useful than its absence.
func describeException(exception *LocalInferenceException) string {
	if exception == nil || exception.Environment == "" {
		return " (no local_inference exception is declared for this profile)"
	}
	return fmt.Sprintf(" (the exception declared for environment %q allows %v)", exception.Environment, exception.AllowedProviders)
}
