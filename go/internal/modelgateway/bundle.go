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
	Profile            string              `json:"profile" yaml:"profile"`
	Candidates         []CandidateDoc      `json:"candidates" yaml:"candidates"`
	RoutingConstraints *RoutingConstraints `json:"routing_constraints,omitempty" yaml:"routing_constraints,omitempty"`
}

// CandidateDoc is one candidate entry within a profile — the subset of model_profile.schema.json's
// candidate fields the router actually needs, plus the pricing fields OBS-003's FinOps ledger
// reads (CostModel/CostPerMillion*Tokens); tool_calling/caching_capability/max_context_tokens are
// documented there but not yet consumed here (see roadmap.md MDL-002).
type CandidateDoc struct {
	Provider                   string  `json:"provider" yaml:"provider"`
	Model                      string  `json:"model" yaml:"model"`
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
}

// ErrProfileNotFound is returned when a requested profile has no entry in the bundle.
var ErrProfileNotFound = errors.New("modelgateway: profile not found in ModelPolicyBundle")

// ResolveProfile returns the candidates (in whatever order the bundle declares them — Decide sorts
// by Priority itself) and routing data_sensitivity, if any, for a named capability profile. A
// caller never names a concrete model — only a profile (docs/adr/0004) — so an unresolvable
// profile is a real, reportable error, not a silent empty candidate list.
func (doc ModelPolicyBundleDoc) ResolveProfile(profile string) ([]Candidate, string, error) {
	for _, p := range doc.Profiles {
		if p.Profile != profile {
			continue
		}
		candidates := make([]Candidate, len(p.Candidates))
		for i, c := range p.Candidates {
			candidates[i] = Candidate{Provider: c.Provider, Model: c.Model, Priority: c.Priority}
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
