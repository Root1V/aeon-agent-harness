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
	CostModel *string
	// PromptTokens/CompletionTokens are nil when the provider reported no usage (MDL-014). Stored as
	// NULL rather than 0, because a call whose consumption nobody measured is not a free call.
	PromptTokens     *int
	CompletionTokens *int
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
	// ProviderRequestID is the platform's own id for this call (OBS-007), and the only key that can
	// join this row to the platform's accounting for the same call. Empty for a provider that issues
	// none, stored as NULL.
	ProviderRequestID string
	// IdempotentReplayOf names the generation that was billed, when this call was served as a replay
	// (OBS-006). Empty for a real generation.
	IdempotentReplayOf string
	// ServedModel is the provider's own answer to "which model was this" (OBS-005), which can differ
	// from Model — the bundle's, which is what the cost is imputed to. Empty when the provider does not
	// say; stored NULL rather than echoing Model back, because "served what we asked" and "never said"
	// are different facts.
	ServedModel string
	// ServedByInstance is which deployment answered (OBS-005). The question an incident starts from,
	// and recorded nowhere before this.
	ServedByInstance string
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
	var requestID *string
	if entry.ProviderRequestID != "" {
		requestID = &entry.ProviderRequestID
	}
	var replayOf *string
	if entry.IdempotentReplayOf != "" {
		replayOf = &entry.IdempotentReplayOf
	}
	_, err := l.pool.Exec(ctx,
		`INSERT INTO model_gateway_costs (provider, model, cost_model, prompt_tokens, completion_tokens, cost_usd, run_id, agent_manifest_ref, cache_read_tokens, cache_write_tokens, reasoning_tokens, provider_request_id, idempotent_replay_of, served_model, served_by_instance)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		entry.Provider, entry.Model, entry.CostModel, entry.PromptTokens, entry.CompletionTokens, entry.CostUSD, runID, agentRef,
		entry.CacheReadTokens, entry.CacheWriteTokens, entry.ReasoningTokens, requestID, replayOf, nullable(entry.ServedModel), nullable(entry.ServedByInstance),
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

// AttributionTotal is one row of TotalsByRun/TotalsByAgent: what a single run, or a single agent
// manifest, actually cost.
//
// THE OTHER HALF OF OBS-003'S OWN TITLE. TotalsByModel answers "which model costs us most", which is
// a purchasing question. This answers "what did this run cost" and "what does this agent cost us",
// which is the question anyone running agents actually asks — and it had no data until OBS-003b, not
// because the columns were missing but because no caller ever filled them.
type AttributionTotal struct {
	// Key is the run id, or the agent manifest ref, and nil for the group of calls that named
	// NEITHER.
	//
	// NIL IS A REPORTED GROUP, NOT A FILTERED-OUT ONE, and that is the whole reason this type does not
	// use a plain string. A call made outside any run is legitimate — a script hitting /decide, an
	// eval — so those rows exist and their cost is real. Dropping them would make this page's total
	// silently smaller than TotalsByModel's over the same ledger, with nothing on either page saying
	// why, and the reader would conclude the cheaper number is the true one.
	Key           *string  `json:"key"`
	CallCount     int64    `json:"call_count"`
	TotalCostUSD  *float64 `json:"total_cost_usd"`
	UnpricedCalls int64    `json:"unpriced_calls"`
	// DistinctModels is how many (provider, model) pairs this run or agent used. A run that fell back
	// to a second provider mid-flight is a different thing from one that used a single model, and
	// without this the cost is a number with no shape.
	DistinctModels        int64 `json:"distinct_models"`
	TotalPromptTokens     int64 `json:"total_prompt_tokens"`
	TotalCompletionTokens int64 `json:"total_completion_tokens"`
}

// totalsByAttribution runs the shared aggregation over one nullable column. column is a fixed
// identifier chosen by the two callers below, never anything a request supplies.
func (l *FinOpsLedger) totalsByAttribution(ctx context.Context, column string) ([]AttributionTotal, error) {
	rows, err := l.pool.Query(ctx,
		`SELECT `+column+`, count(*), sum(cost_usd), count(*) FILTER (WHERE cost_usd IS NULL),
		        count(DISTINCT (provider, model)),
		        coalesce(sum(prompt_tokens), 0), coalesce(sum(completion_tokens), 0)
		 FROM model_gateway_costs
		 GROUP BY `+column+`
		 -- NULLS LAST on the GROUP key as well as the sum: the unattributed group is reported, and it
		 -- belongs at the bottom rather than sorted in among named runs as if it were one of them.
		 ORDER BY sum(cost_usd) DESC NULLS LAST, `+column+` NULLS LAST`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: aggregating model gateway costs by %s: %w", column, err)
	}
	defer rows.Close()

	var out []AttributionTotal
	for rows.Next() {
		var t AttributionTotal
		if err := rows.Scan(&t.Key, &t.CallCount, &t.TotalCostUSD, &t.UnpricedCalls, &t.DistinctModels,
			&t.TotalPromptTokens, &t.TotalCompletionTokens); err != nil {
			return nil, fmt.Errorf("store: scanning cost total by %s: %w", column, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TotalsByRun is the cost of each run (OBS-003b). The nil-key row is the calls that named no run.
func (l *FinOpsLedger) TotalsByRun(ctx context.Context) ([]AttributionTotal, error) {
	return l.totalsByAttribution(ctx, "run_id")
}

// TotalsByAgent is the cost of each agent manifest (OBS-003b). The nil-key row is the calls that
// named no agent — which is NOT the same set as TotalsByRun's: a run can identify itself and still
// not say which agent it runs as, and before OBS-003b every Deep Research run did exactly that.
func (l *FinOpsLedger) TotalsByAgent(ctx context.Context) ([]AttributionTotal, error) {
	return l.totalsByAttribution(ctx, "agent_manifest_ref")
}

// LedgerRow is one recorded cost event, read back for reconciliation (OBS-007).
type LedgerRow struct {
	Provider string
	Model    string
	// Nullable on the way OUT too (MDL-014), not just on the way in. Scanning a NULL into an int would
	// turn it back into 0 on every read and undo the whole feature at the last step — the fabricated
	// zero would simply move from the write path to the read path.
	PromptTokens       *int
	CompletionTokens   *int
	CostUSD            *float64
	ProviderRequestID  string
	IdempotentReplayOf string
	ServedModel        string
	ServedByInstance   string
}

// RowsWithProviderRequestID returns the rows that CAN be reconciled: those carrying the platform's
// own id for the call. Rows without one are excluded rather than reported as matching, since a row
// with no join key is not in agreement with the platform — it is simply unchecked, and counting it as
// agreement is how a reconciliation reports success over data it never compared.
func (l *FinOpsLedger) RowsWithProviderRequestID(ctx context.Context, provider string, limit int) ([]LedgerRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := l.pool.Query(ctx,
		`SELECT provider, model, prompt_tokens, completion_tokens, cost_usd, provider_request_id, coalesce(idempotent_replay_of, ''), coalesce(served_model, ''), coalesce(served_by_instance, '')
		   FROM model_gateway_costs
		  WHERE provider = $1 AND provider_request_id IS NOT NULL
		  ORDER BY id DESC
		  LIMIT $2`, provider, limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading reconcilable cost rows: %w", err)
	}
	defer rows.Close()

	var out []LedgerRow
	for rows.Next() {
		var r LedgerRow
		if err := rows.Scan(&r.Provider, &r.Model, &r.PromptTokens, &r.CompletionTokens, &r.CostUSD, &r.ProviderRequestID, &r.IdempotentReplayOf, &r.ServedModel, &r.ServedByInstance); err != nil {
			return nil, fmt.Errorf("store: scanning cost row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RowsForModel returns the recorded rows for one (provider, model), newest first.
//
// Separate from RowsWithProviderRequestID because that one filters to what can be RECONCILED (a row
// with the platform's id) and this one answers "what did we record for this model" — including rows
// from a provider that issues no request id at all. Folding them into one reader would mean one of the
// two questions silently getting the other's answer.
func (l *FinOpsLedger) RowsForModel(ctx context.Context, provider, model string, limit int) ([]LedgerRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := l.pool.Query(ctx,
		`SELECT provider, model, prompt_tokens, completion_tokens, cost_usd,
		        coalesce(provider_request_id, ''), coalesce(idempotent_replay_of, ''),
		        coalesce(served_model, ''), coalesce(served_by_instance, '')
		   FROM model_gateway_costs
		  WHERE provider = $1 AND model = $2
		  ORDER BY id DESC
		  LIMIT $3`, provider, model, limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading cost rows for %s/%s: %w", provider, model, err)
	}
	defer rows.Close()

	var out []LedgerRow
	for rows.Next() {
		var r LedgerRow
		if err := rows.Scan(&r.Provider, &r.Model, &r.PromptTokens, &r.CompletionTokens, &r.CostUSD,
			&r.ProviderRequestID, &r.IdempotentReplayOf, &r.ServedModel, &r.ServedByInstance); err != nil {
			return nil, fmt.Errorf("store: scanning cost row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// nullable stores an empty string as SQL NULL. An absent fact and a fact that is the empty string are
// not the same, and only NULL says the first one.
func nullable(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// RunSpend is what one run has cost so far, as the ledger can honestly report it.
//
// THREE NUMBERS AND NOT ONE, for the reason OBS-008 and OBS-009 exist: a run whose calls include some
// the gateway could not price has a cost that is a LOWER BOUND, and a single float cannot say so.
// `UnpricedCalls` is what makes "you are at $4.90 of $5.00" distinguishable from "you are at $4.90 of
// $5.00 plus twelve calls nobody could price".
type RunSpend struct {
	CostUSD       float64 `json:"cost_usd"`
	PricedCalls   int64   `json:"priced_calls"`
	UnpricedCalls int64   `json:"unpriced_calls"`
	ModelCalls    int64   `json:"model_calls"`
	// Tokens is prompt + completion across the run (MDL-018). Summed from the same rows as the cost,
	// so a token ceiling and a cost ceiling cannot disagree about what a run did.
	//
	// It is a LOWER BOUND for the same reason the cost is: a provider that reports no usage leaves
	// nulls, which sum to nothing rather than to a guess. UnreportedUsageCalls is how many.
	Tokens               int64 `json:"tokens"`
	UnreportedUsageCalls int64 `json:"unreported_usage_calls"`
}

// SpendForRun sums one run's recorded cost (MDL-017).
//
// Reads the ledger rather than keeping a counter in memory, and that is the point: the gateway is
// restartable and horizontally scalable, so an in-process counter would reset on a deploy and be wrong
// per replica — a ceiling that forgets is not a ceiling. The ledger is the same table the FinOps
// dashboard aggregates, so the number enforced is the number an operator sees.
func (l *FinOpsLedger) SpendForRun(ctx context.Context, runID string) (RunSpend, error) {
	var s RunSpend
	var sum *float64
	err := l.pool.QueryRow(ctx,
		`SELECT coalesce(sum(cost_usd), 0), count(*) FILTER (WHERE cost_usd IS NOT NULL),
		        count(*) FILTER (WHERE cost_usd IS NULL), count(*),
		        coalesce(sum(coalesce(prompt_tokens, 0) + coalesce(completion_tokens, 0)), 0),
		        count(*) FILTER (WHERE prompt_tokens IS NULL AND completion_tokens IS NULL)
		 FROM model_gateway_costs WHERE run_id = $1`, runID,
	).Scan(&sum, &s.PricedCalls, &s.UnpricedCalls, &s.ModelCalls, &s.Tokens, &s.UnreportedUsageCalls)
	if err != nil {
		return RunSpend{}, fmt.Errorf("store: summing spend for run %s: %w", runID, err)
	}
	if sum != nil {
		s.CostUSD = *sum
	}
	return s, nil
}
