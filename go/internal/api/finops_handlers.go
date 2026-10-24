package api

import (
	"fmt"
	"html"
	"net/http"

	"github.com/aeon-ai/aeon/go/internal/auth"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// FinOpsHandlers is OBS-003's dashboard: a real, server-rendered HTML page aggregating real cost
// events ModelGatewayHandlers.recordCost wrote to FinOpsLedger — real SQL sums, not numbers made up
// for the page.
//
// This doc used to claim that a compute_based model "reading $0.00 total is honestly
// distinguishable" from one that is genuinely free, because the row also shows its cost_model. That
// was the defect OBS-008 fixed, written down as a property: it asked the reader to know that
// compute_based implies unpriced, printed the same $0.00 either way, and added it to the grand
// total regardless. An unpriced call now renders as "—" and is counted separately, so the page
// never puts a figure where it has none.
type FinOpsHandlers struct {
	// VRT-AEON-005 T-2: the store. GET /finops/costs sums the CALLER'S tenant and nothing else —
	// cost per case is a business and audit fact, and another project must not be able to read it.
	Ledger *store.Store
}

// runCostResponse is FIN-002: one run's spend as DATA (`VRT-AEON-007`, point 2).
//
// Veritium's argument for it, and it is the one that decides against their own alternative of leaving
// the HTML as the only view: their `HttpModel` discards the `aeon` block of each response — correctly,
// because it belongs to one specific harness — so a consumer that wants to show "this case cost X" was
// left reading an HTML page or our database. `GET /finops/costs` is a dashboard and was never an API:
// anything consuming it is parsing a screen.
type runCostResponse struct {
	RunID string `json:"run_id"`
	// CostUSD IS A POINTER, and this is the whole three-state rule of this ledger restated at the
	// response boundary: a number means priced, and NULL means "every call we recorded has no price",
	// which is a different fact from free. The HTML prints "—" for it; JSON has a word for it.
	CostUSD    *float64 `json:"cost_usd"`
	ModelCalls int64    `json:"model_calls"`
	// PricedCalls plus UnpricedCalls equals ModelCalls. Both are reported because a partially-priced
	// run's cost is a LOWER BOUND, and a consumer showing it as a figure should be able to say so.
	PricedCalls   int64 `json:"priced_calls"`
	UnpricedCalls int64 `json:"unpriced_calls"`
	// DiscardedCalls and DiscardedCostUSD are attempts this gateway paid for and threw away — an
	// answer the model produced and that was unusable, so another candidate served instead (FIN-003).
	//
	// CostUSD AND THE TOKEN COUNTERS INCLUDE THEM, because the money left; ModelCalls does not,
	// because they are not calls anyone was served. So a consumer can say "this case cost X, of which
	// Y was wasted" — the alternative was hiding the waste or counting it as work.
	DiscardedCalls   int64   `json:"discarded_calls"`
	DiscardedCostUSD float64 `json:"discarded_cost_usd"`
	// PromptTokens and CompletionTokens are lower bounds too: a provider that reports no usage leaves
	// nulls, which sum to nothing rather than to a guess. UnreportedUsageCalls is how many did that.
	PromptTokens         int64 `json:"prompt_tokens"`
	CompletionTokens     int64 `json:"completion_tokens"`
	Tokens               int64 `json:"tokens"`
	UnreportedUsageCalls int64 `json:"unreported_usage_calls"`
	DistinctModels       int64 `json:"distinct_models"`
}

// AttributionTotalView is store.AttributionTotal under the name this page uses. Aliased rather than
// redeclared: a parallel struct here would be one more place for the nil-key convention to be
// forgotten, and the nil key is the part of this data that must not be dropped.
type AttributionTotalView = store.AttributionTotal

// Register mounts the FinOps routes on mux.
func (h *FinOpsHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /finops/costs", h.showCosts)
	mux.HandleFunc("GET /finops/runs/{run_id}", h.showRunCost)
}

// showRunCost answers one run's spend as JSON, scoped to the caller's tenant (FIN-002).
//
// 404 AND 200-WITH-ZEROS ARE DIFFERENT ANSWERS, and separating them is the only part of this that is
// not a sum. The ledger holds no row for a run that never called a model, and no row for a run id
// that does not exist — the same silence for two different facts. A run refused by policy or by its
// budget before its first model call is exactly the second case, and reporting it as "no such run"
// would make a governed-and-refused run indistinguishable from a typo, which is the opposite of what
// an operator is looking at this page for. So the journal is asked whether the run is known at all.
//
// Veritium's T-7 holds either way: both the ledger and the journal are scoped to the caller's tenant,
// so another tenant's run is 404 because to this caller it does not exist — and that answer carries
// no information about whether it exists elsewhere.
func (h *FinOpsHandlers) showRunCost(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.CallerFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthenticated: a run's cost is scoped to the caller's tenant (VRT-AEON-005)",
		})
		return
	}
	runID := r.PathValue("run_id")
	spend, err := h.Ledger.FinOpsLedgerFor(caller.Tenant).SpendForRun(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// ModelCalls AND DiscardedCalls, because a run whose only trace is an attempt we paid for and
	// threw away is a run that exists and spent money (FIN-003) — asking the journal about it would
	// be asking the wrong witness.
	if spend.ModelCalls == 0 && spend.DiscardedCalls == 0 {
		known, err := h.Ledger.CheckpointerFor(caller.Tenant).KnownRun(r.Context(), runID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !known {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error":  "no such run for this tenant",
				"run_id": runID,
			})
			return
		}
	}

	response := runCostResponse{
		RunID: runID, ModelCalls: spend.ModelCalls,
		PricedCalls: spend.PricedCalls, UnpricedCalls: spend.UnpricedCalls,
		DiscardedCalls: spend.DiscardedCalls, DiscardedCostUSD: spend.DiscardedCostUSD,
		PromptTokens: spend.PromptTokens, CompletionTokens: spend.CompletionTokens,
		Tokens: spend.Tokens, UnreportedUsageCalls: spend.UnreportedUsageCalls,
		DistinctModels: spend.DistinctModels,
	}
	// The pointer is left nil when NOTHING was priced, which covers both the run with no calls and the
	// run whose every call is compute_based. `0` is reserved for a run that was really priced at zero.
	// Priced OR discarded-and-priced: a run whose only cost came from a discarded attempt still has a
	// real figure, and leaving it nil would say "no price" about money that was charged.
	if spend.PricedCalls > 0 || spend.DiscardedCostUSD != 0 {
		cost := spend.CostUSD
		response.CostUSD = &cost
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *FinOpsHandlers) showCosts(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.CallerFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthenticated: costs are scoped to the caller's tenant (VRT-AEON-005)",
		})
		return
	}
	ledger := h.Ledger.FinOpsLedgerFor(caller.Tenant)
	totals, err := ledger.TotalsByModel(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// OBS-003b: read before anything is written, so a failure here is a 500 and not a half-rendered
	// page with a 200 already on the wire.
	byRun, err := ledger.TotalsByRun(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	byAgent, err := ledger.TotalsByAgent(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Aeon FinOps — cost per model, run and agent</title></head>
<body>
<h1>Cost per model</h1>
`)

	if len(totals) == 0 {
		fmt.Fprint(w, "<p>no cost events recorded yet</p>\n")
		fmt.Fprint(w, "</body></html>\n")
		return
	}

	var grandTotalUSD float64
	var unpricedCalls, discardedCalls int64
	fmt.Fprint(w, "<table><tr><th>provider</th><th>model</th><th>cost_model</th><th>calls</th><th>total cost (USD)</th><th>unpriced calls</th><th>prompt tokens</th><th>completion tokens</th><th>discarded attempts</th></tr>\n")
	for _, t := range totals {
		if t.TotalCostUSD != nil {
			grandTotalUSD += *t.TotalCostUSD
		}
		unpricedCalls += t.UnpricedCalls
		discardedCalls += t.DiscardedCalls
		fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td><td>%d</td><td>%d</td><td>%d</td><td>%d</td></tr>\n",
			html.EscapeString(t.Provider), html.EscapeString(t.Model), html.EscapeString(orUnknown(t.CostModel)),
			t.CallCount, costCell(t.TotalCostUSD), t.UnpricedCalls, t.TotalPromptTokens, t.TotalCompletionTokens,
			t.DiscardedCalls)
	}
	fmt.Fprint(w, "</table>\n")

	// The grand total is deliberately not presented as "the" cost when part of it is unknown.
	// Printing one number over a partially-unpriced ledger is the same lie as the zero, one level
	// up: a lower bound wearing the shape of an exact figure.
	if unpricedCalls > 0 {
		fmt.Fprintf(w, "<p>Total of priced calls: $%.4f — plus %d call(s) whose cost is unknown, not zero</p>\n",
			grandTotalUSD, unpricedCalls)
	} else {
		fmt.Fprintf(w, "<p>Total: $%.4f</p>\n", grandTotalUSD)
	}
	// FIN-003: said in a sentence rather than left to a column nobody adds up. This figure is inside
	// the total above — it is money that was spent on answers nobody received, and before this change
	// it was not in the total OR in the table, because the row did not exist.
	if discardedCalls > 0 {
		fmt.Fprintf(w, "<p>Of that, %d attempt(s) were generated, billed and discarded: their answer "+
			"was unusable and another candidate served instead. The tokens are real and are included above.</p>\n",
			discardedCalls)
	}

	// OBS-003b: the two attributions the spec's own title for OBS-003 asked for and that had no data
	// to render until the Python callers started naming themselves.
	writeAttribution(w, "Cost per run", "run", byRun)
	writeAttribution(w, "Cost per agent", "agent_manifest_ref", byAgent)

	fmt.Fprint(w, "</body></html>\n")
}

// writeAttribution renders one attribution table (per run, or per agent).
//
// THE UNATTRIBUTED ROW IS PRINTED, and it is printed as a sentence rather than as a table row with an
// empty first cell. A blank cell is indistinguishable from a rendering bug, and this group is the one
// thing on the page a reader must not mistake for noise: while it is large, every other number here
// is a lower bound. Before OBS-003b it was the ONLY group — every call ever recorded landed in it.
func writeAttribution(w http.ResponseWriter, heading, keyLabel string, totals []AttributionTotalView) {
	fmt.Fprintf(w, "<h1>%s</h1>\n", html.EscapeString(heading))
	if len(totals) == 0 {
		fmt.Fprint(w, "<p>no cost events recorded yet</p>\n")
		return
	}

	var unattributed *AttributionTotalView
	named := make([]AttributionTotalView, 0, len(totals))
	for i := range totals {
		if totals[i].Key == nil {
			unattributed = &totals[i]
			continue
		}
		named = append(named, totals[i])
	}

	if len(named) == 0 {
		fmt.Fprintf(w, "<p>no call has named a %s yet</p>\n", html.EscapeString(keyLabel))
	} else {
		// `discarded` is its own column and not folded into `calls` (FIN-003): the cost column covers
		// both because the money is real, and a reader who cannot see the split would compute an
		// average per call over attempts that served nobody.
		fmt.Fprintf(w, "<table><tr><th>%s</th><th>calls</th><th>models</th><th>total cost (USD)</th><th>unpriced calls</th><th>prompt tokens</th><th>completion tokens</th><th>discarded attempts</th></tr>\n",
			html.EscapeString(keyLabel))
		for _, t := range named {
			fmt.Fprintf(w, "<tr><td>%s</td><td>%d</td><td>%d</td><td>%s</td><td>%d</td><td>%d</td><td>%d</td><td>%d</td></tr>\n",
				html.EscapeString(*t.Key), t.CallCount, t.DistinctModels, costCell(t.TotalCostUSD),
				t.UnpricedCalls, t.TotalPromptTokens, t.TotalCompletionTokens, t.DiscardedCalls)
		}
		fmt.Fprint(w, "</table>\n")
	}

	if unattributed != nil {
		fmt.Fprintf(w, "<p>%d call(s) named no %s and are not in the table above — their cost is real and attributed to nobody, so every figure here is a lower bound (%s so far).</p>\n",
			unattributed.CallCount, html.EscapeString(keyLabel), costPhrase(unattributed.TotalCostUSD))
	}
}

// costPhrase words an unattributed group's cost, including the case where none of it was priced —
// which must not read as "$0.00 unattributed", the same mistake OBS-008 removed one table up.
func costPhrase(usd *float64) string {
	if usd == nil {
		return "none of it priced"
	}
	return fmt.Sprintf("$%.4f", *usd)
}

// costCell renders a group's cost, or an em dash when nothing in it was priced. Rendering 0.0000
// there is exactly what OBS-008 removed: it is read as a measurement.
func costCell(usd *float64) string {
	if usd == nil {
		return "&mdash;"
	}
	return fmt.Sprintf("%.4f", *usd)
}

// orUnknown names an absent cost_model rather than leaving the cell blank, which is
// indistinguishable from a rendering bug.
func orUnknown(s *string) string {
	if s == nil || *s == "" {
		return "unknown"
	}
	return *s
}
