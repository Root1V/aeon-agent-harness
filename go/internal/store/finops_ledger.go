package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CostEntry is one real model-gateway call's cost, as recorded by FinOpsLedger.Record (OBS-003).
type CostEntry struct {
	Provider string
	Model    string
	// CostModel is "token_based" | "compute_based", and nil when no rate is configured for this
	// (provider, model) at all — we then do not know how it bills, and naming one would invent it.
	CostModel        *string
	PromptTokens     int
	CompletionTokens int
	// CostUSD is nil when nobody computed a cost: a compute_based provider billed by GPU-second, or
	// a model with no configured rate. A 0 here must mean "computed, and it was zero" (OBS-008).
	CostUSD          *float64
	RunID            string // optional — empty when the caller didn't supply one
	AgentManifestRef string // optional — empty when the caller didn't supply one
	// CacheReadTokens is the subset of PromptTokens served from cache, and CacheWriteTokens what
	// was written to it (MDL-012). Both nil when the provider reported no cache accounting at all —
	// see schema.sql for why that is not the same as zero.
	CacheReadTokens  *int
	CacheWriteTokens *int
	// ReasoningTokens is the slice of the output spent thinking (MDL-016). Nil when the provider
	// does not break it out — recording a zero would claim it measured and found none.
	ReasoningTokens *int
}

// ModelTotal is one row of TotalsByModel's real SQL aggregation.
type ModelTotal struct {
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	CostModel *string `json:"cost_model"`
	CallCount int64   `json:"call_count"`
	// TotalCostUSD is the sum over the calls in this group that HAVE a cost, and nil when none of
	// them does. Nil is the case OBS-008 exists for: every prometheus_inference call used to land
	// here as $0.00, which reads as "this was free" rather than "this was never priced".
	TotalCostUSD *float64 `json:"total_cost_usd"`
	// UnpricedCalls is how many of CallCount contributed nothing to TotalCostUSD. Without it a
	// partially-priced group returns a lower bound wearing the shape of an exact figure — the same
	// mistake as the zero, one aggregation up.
	UnpricedCalls         int64 `json:"unpriced_calls"`
	TotalPromptTokens     int64 `json:"total_prompt_tokens"`
	TotalCompletionTokens int64 `json:"total_completion_tokens"`
	// Cache totals sum only the calls that reported cache accounting; a provider that reports none
	// contributes nothing rather than zeros. The row-level distinction between "not reported" and
	// "nothing cached" survives in the table — it cannot survive an aggregate, so read these as
	// "of what was measured", not "of everything".
	TotalCacheReadTokens  int64 `json:"total_cache_read_tokens"`
	TotalCacheWriteTokens int64 `json:"total_cache_write_tokens"`
	TotalReasoningTokens  int64 `json:"total_reasoning_tokens"`
}

// FinOpsLedger is the Postgres-backed cost ledger (OBS-003).
type FinOpsLedger struct {
	pool *pgxpool.Pool
}

// Record writes one real cost event. A nil RunID/AgentManifestRef in entry is stored as SQL NULL,
// not an empty string — "unknown which run" and "known to be a run with an empty id" are different
// facts, and NULL is what TotalsByRun (a future consumer) would need to distinguish them.
//
// A nil CostUSD/CostModel is stored as NULL for the same reason (OBS-008), and this is the write
// that must happen even when nothing about the cost is known: a call that leaves no row is worse
// than a row with a null price, because a null can be audited and an absence cannot.
func (l *FinOpsLedger) Record(ctx context.Context, entry CostEntry) error {
	var runID, agentRef *string
	if entry.RunID != "" {
		runID = &entry.RunID
	}
	if entry.AgentManifestRef != "" {
		agentRef = &entry.AgentManifestRef
	}
	_, err := l.pool.Exec(ctx,
		`INSERT INTO model_gateway_costs (provider, model, cost_model, prompt_tokens, completion_tokens, cost_usd, run_id, agent_manifest_ref, cache_read_tokens, cache_write_tokens, reasoning_tokens)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		entry.Provider, entry.Model, entry.CostModel, entry.PromptTokens, entry.CompletionTokens, entry.CostUSD, runID, agentRef,
		entry.CacheReadTokens, entry.CacheWriteTokens, entry.ReasoningTokens,
	)
	if err != nil {
		return fmt.Errorf("store: recording model gateway cost: %w", err)
	}
	return nil
}

// TotalsByModel is OBS-003's real dashboard aggregation: total real cost, call count, and token
// usage grouped by (provider, model) — every provider's own cost_model (token_based|compute_based)
// is carried through unaggregated per group since it's constant per model.
func (l *FinOpsLedger) TotalsByModel(ctx context.Context) ([]ModelTotal, error) {
	rows, err := l.pool.Query(ctx,
		`SELECT provider, model, cost_model, count(*), sum(cost_usd), count(*) FILTER (WHERE cost_usd IS NULL),
		        coalesce(sum(prompt_tokens), 0), coalesce(sum(completion_tokens), 0),
		        coalesce(sum(cache_read_tokens), 0), coalesce(sum(cache_write_tokens), 0),
		        coalesce(sum(reasoning_tokens), 0)
		 FROM model_gateway_costs
		 GROUP BY provider, model, cost_model
		 ORDER BY sum(cost_usd) DESC NULLS LAST`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: aggregating model gateway costs: %w", err)
	}
	defer rows.Close()

	var out []ModelTotal
	for rows.Next() {
		var t ModelTotal
		if err := rows.Scan(&t.Provider, &t.Model, &t.CostModel, &t.CallCount, &t.TotalCostUSD, &t.UnpricedCalls,
			&t.TotalPromptTokens, &t.TotalCompletionTokens,
			&t.TotalCacheReadTokens, &t.TotalCacheWriteTokens, &t.TotalReasoningTokens); err != nil {
			return nil, fmt.Errorf("store: scanning model gateway cost total: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
