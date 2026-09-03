// Package finops is OBS-003: real dollar-cost computation for model calls from real token usage
// and a real, config-as-code pricing table — no dependency on go/internal/modelgateway, so it stays
// independently testable and usable anywhere a (provider, model, usage) triple is available.
package finops

// Rate is real, config-as-code pricing for one provider+model pair, sourced from
// proto/schemas/model_profile.schema.json's cost_model/cost_per_million_*_tokens fields (the same
// ModelPolicyBundle candidates already declare provider/model/priority in) — see
// go/cmd/aeon-modelgw/main.go for how a real Rate slice is built from that file.
type Rate struct {
	Provider            string
	Model               string
	CostModel           string // "token_based" | "compute_based" — mirrors providers.Provider.CostModel()
	InputPerMillionUSD  float64
	OutputPerMillionUSD float64
}

// PricingTable resolves (provider, model) to a Rate.
type PricingTable struct {
	rates map[string]Rate
}

func key(provider, model string) string { return provider + "/" + model }

// NewPricingTable builds a table from rates. A later entry for the same (Provider, Model) pair
// replaces an earlier one.
func NewPricingTable(rates []Rate) *PricingTable {
	t := &PricingTable{rates: make(map[string]Rate, len(rates))}
	for _, r := range rates {
		t.rates[key(r.Provider, r.Model)] = r
	}
	return t
}

// Rate returns the configured Rate for (provider, model), if any.
func (t *PricingTable) Rate(provider, model string) (Rate, bool) {
	if t == nil {
		return Rate{}, false
	}
	r, ok := t.rates[key(provider, model)]
	return r, ok
}

// CostUSD computes the real dollar cost of one model call from real token usage. ok is false when
// no rate is configured for (provider, model), or when its cost_model isn't "token_based" (a
// compute_based provider like prometheus_inference is billed by GPU-seconds, which this package
// doesn't track — see backlog.md) — callers must treat that as "cost unknown", never silently as
// $0, since those are very different facts for a FinOps dashboard to show.
func (t *PricingTable) CostUSD(provider, model string, promptTokens, completionTokens int) (usd float64, ok bool) {
	r, found := t.Rate(provider, model)
	if !found || r.CostModel != "token_based" {
		return 0, false
	}
	usd = float64(promptTokens)/1_000_000*r.InputPerMillionUSD + float64(completionTokens)/1_000_000*r.OutputPerMillionUSD
	return usd, true
}
