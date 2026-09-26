package finops

import (
	"context"
	"fmt"
	"math"
)

// PlatformUsage is one call as the PLATFORM accounts for it. Deliberately not the provider's own
// struct: this package must not import a concrete adapter (MDL-017 removed the last provider name
// from the routing core for the same reason), so the caller adapts.
type PlatformUsage struct {
	RequestID        string
	Model            string
	PromptTokens     int
	CompletionTokens int
	// CostUSD is nil when the platform priced nothing. It really happens — eleven rows on the
	// reference deployment have no cost, created when two modalities were added to the registry and
	// not to the price table, and deliberately left null rather than rewritten.
	CostUSD *float64
	// PromptPricePer1M / CompletionPricePer1M are the rates ACTUALLY APPLIED. They are what turns this
	// from copying a number into comparing two derivations: the cost can be recomputed from them and
	// checked against what the platform reported, independently of anything we believe.
	PromptPricePer1M     *float64
	CompletionPricePer1M *float64
}

// UsageSource fetches the platform's accounting for one request.
type UsageSource interface {
	PlatformUsage(ctx context.Context, requestID string) (*PlatformUsage, error)
}

// LedgerRow is one call as WE account for it.
type LedgerRow struct {
	Provider          string
	Model             string
	PromptTokens      int
	CompletionTokens  int
	CostUSD           *float64
	ProviderRequestID string
}

// Divergence is one disagreement between the two accountings. The type carries the kind rather than
// only a message, so a caller can act on a token mismatch differently from an unpriced row.
type Divergence struct {
	RequestID string
	Kind      string // "tokens" | "cost" | "model" | "self_inconsistent" | "unpriced_here" | "unpriced_there" | "missing"
	Detail    string
}

// Report is what one reconciliation run found.
type Report struct {
	Compared     int
	Agreed       int
	Divergences  []Divergence
	Unreconciled int // rows that could not be compared at all
}

// costTolerance is the epsilon for comparing two dollar figures.
//
// Not arbitrary: the platform's own cost_usd and a recomputation from its own published rates differ
// by ~1.7e-21 on a real call, which is IEEE754 noise and not a disagreement. A tolerance below that
// would report every single row as divergent, which is the failure mode that gets a reconciliation
// switched off within a day.
const costTolerance = 1e-12

// Reconcile compares our ledger rows against the platform's accounting for the same calls.
//
// What this does NOT claim, and it matters more than the arithmetic: agreement here does not mean
// the spend is correct. The platform's rates are PRICES aligned with published rates for hosted open
// models, not what its own GPUs cost — the cost-derived path exists but is a manual per-model action
// by an operator. So two sides matching to the cent means our derivation agrees with theirs, which is
// worth knowing and is the only thing a client can check. It does not mean anyone has validated the
// price against infrastructure. Told to us by Axonium on 2026-09-22, and recorded here because a
// dashboard showing a tidy match invites exactly the stronger conclusion.
func Reconcile(ctx context.Context, src UsageSource, rows []LedgerRow) (*Report, error) {
	report := &Report{}

	for _, row := range rows {
		if row.ProviderRequestID == "" {
			// Not an agreement and not a divergence: there is no key to compare on. Counting it as
			// either would be a figure about data that was never looked at.
			report.Unreconciled++
			continue
		}

		platform, err := src.PlatformUsage(ctx, row.ProviderRequestID)
		if err != nil {
			report.Unreconciled++
			report.Divergences = append(report.Divergences, Divergence{
				RequestID: row.ProviderRequestID, Kind: "missing",
				Detail: fmt.Sprintf("the platform has no accounting for this call: %v", err),
			})
			continue
		}

		report.Compared++
		before := len(report.Divergences)

		if row.Model != platform.Model {
			report.Divergences = append(report.Divergences, Divergence{
				RequestID: row.ProviderRequestID, Kind: "model",
				Detail: fmt.Sprintf("we recorded model %q, the platform billed %q", row.Model, platform.Model),
			})
		}

		if row.PromptTokens != platform.PromptTokens || row.CompletionTokens != platform.CompletionTokens {
			report.Divergences = append(report.Divergences, Divergence{
				RequestID: row.ProviderRequestID, Kind: "tokens",
				Detail: fmt.Sprintf("we recorded %d prompt / %d completion, the platform %d / %d",
					row.PromptTokens, row.CompletionTokens, platform.PromptTokens, platform.CompletionTokens),
			})
		}

		// The platform's own figure against a recomputation from the platform's own published rates.
		// This catches a fault on their side without asking them, which is the whole reason the rates
		// travelling per request is useful.
		if platform.CostUSD != nil && platform.PromptPricePer1M != nil && platform.CompletionPricePer1M != nil {
			recomputed := (float64(platform.PromptTokens)**platform.PromptPricePer1M +
				float64(platform.CompletionTokens)**platform.CompletionPricePer1M) / 1e6
			if math.Abs(*platform.CostUSD-recomputed) > costTolerance {
				report.Divergences = append(report.Divergences, Divergence{
					RequestID: row.ProviderRequestID, Kind: "self_inconsistent",
					Detail: fmt.Sprintf("the platform's cost %g does not match its own rates applied to its own tokens (%g)",
						*platform.CostUSD, recomputed),
				})
			}
		}

		switch {
		case row.CostUSD == nil && platform.CostUSD != nil:
			// The case that opened this feature. Our ledger could not price a call the platform
			// priced perfectly well — which, for prometheus_inference, is every call.
			report.Divergences = append(report.Divergences, Divergence{
				RequestID: row.ProviderRequestID, Kind: "unpriced_here",
				Detail: fmt.Sprintf("we recorded no cost; the platform charged %g", *platform.CostUSD),
			})
		case row.CostUSD != nil && platform.CostUSD == nil:
			report.Divergences = append(report.Divergences, Divergence{
				RequestID: row.ProviderRequestID, Kind: "unpriced_there",
				Detail: fmt.Sprintf("we recorded %g; the platform has no price for this call", *row.CostUSD),
			})
		case row.CostUSD != nil && platform.CostUSD != nil &&
			math.Abs(*row.CostUSD-*platform.CostUSD) > costTolerance:
			report.Divergences = append(report.Divergences, Divergence{
				RequestID: row.ProviderRequestID, Kind: "cost",
				Detail: fmt.Sprintf("we recorded %g, the platform charged %g", *row.CostUSD, *platform.CostUSD),
			})
		}

		if len(report.Divergences) == before {
			report.Agreed++
		}
	}
	return report, nil
}
