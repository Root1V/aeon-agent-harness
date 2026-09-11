package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CostEntry is one real model-gateway call's cost, as recorded by FinOpsLedger.Record (OBS-003).
type CostEntry struct {
	Provider         string
	Model            string
	CostModel        string // "token_based" | "compute_based"
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	RunID            string // optional — empty when the caller didn't supply one
	AgentManifestRef string // optional — empty when the caller didn't supply one
	// CacheReadTokens is the subset of PromptTokens served from cache, and CacheWriteTokens what
	// was written to it (MDL-012). Both nil when the provider reported no cache accounting at all —
	// see schema.sql for why that is not the same as zero.
	CacheReadTokens  *int
	CacheWriteTokens *int
}

// ModelTotal is one row of TotalsByModel's real SQL aggregation.
type ModelTotal struct {
	Provider              string  `json:"provider"`
	Model                 string  `json:"model"`
	CostModel             string  `json:"cost_model"`
	CallCount             int64   `json:"call_count"`
	TotalCostUSD          float64 `json:"total_cost_usd"`
	TotalPromptTokens     int64   `json:"total_prompt_tokens"`
	TotalCompletionTokens int64   `json:"total_completion_tokens"`
	// Cache totals sum only the calls that reported cache accounting; a provider that reports none
	// contributes nothing rather than zeros. The row-level distinction between "not reported" and
	// "nothing cached" survives in the table — it cannot survive an aggregate, so read these as
	// "of what was measured", not "of everything".
	TotalCacheReadTokens  int64 `json:"total_cache_read_tokens"`
	TotalCacheWriteTokens int64 `json:"total_cache_write_tokens"`
}

// FinOpsLedger is the Postgres-backed cost ledger (OBS-003).
type FinOpsLedger struct {
	pool *pgxpool.Pool
}

// Record writes one real cost event. A nil RunID/AgentManifestRef in entry is stored as SQL NULL,
// not an empty string — "unknown which run" and "known to be a run with an empty id" are different
// facts, and NULL is what TotalsByRun (a future consumer) would need to distinguish them.
func (l *FinOpsLedger) Record(ctx context.Context, entry CostEntry) error {
	var runID, agentRef *string
	if entry.RunID != "" {
		runID = &entry.RunID
	}
	if entry.AgentManifestRef != "" {
		agentRef = &entry.AgentManifestRef
	}
	_, err := l.pool.Exec(ctx,
		`INSERT INTO model_gateway_costs (provider, model, cost_model, prompt_tokens, completion_tokens, cost_usd, run_id, agent_manifest_ref, cache_read_tokens, cache_write_tokens)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		entry.Provider, entry.Model, entry.CostModel, entry.PromptTokens, entry.CompletionTokens, entry.CostUSD, runID, agentRef,
		entry.CacheReadTokens, entry.CacheWriteTokens,
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
		`SELECT provider, model, cost_model, count(*), coalesce(sum(cost_usd), 0), coalesce(sum(prompt_tokens), 0), coalesce(sum(completion_tokens), 0),
		        coalesce(sum(cache_read_tokens), 0), coalesce(sum(cache_write_tokens), 0)
		 FROM model_gateway_costs
		 GROUP BY provider, model, cost_model
		 ORDER BY sum(cost_usd) DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: aggregating model gateway costs: %w", err)
	}
	defer rows.Close()

	var out []ModelTotal
	for rows.Next() {
		var t ModelTotal
		if err := rows.Scan(&t.Provider, &t.Model, &t.CostModel, &t.CallCount, &t.TotalCostUSD, &t.TotalPromptTokens, &t.TotalCompletionTokens,
			&t.TotalCacheReadTokens, &t.TotalCacheWriteTokens); err != nil {
			return nil, fmt.Errorf("store: scanning model gateway cost total: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
