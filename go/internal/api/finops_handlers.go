package api

import (
	"fmt"
	"html"
	"net/http"

	"github.com/aeon-ai/aeon/go/internal/store"
)

// FinOpsHandlers is OBS-003's dashboard: a real, server-rendered HTML page aggregating real cost
// events ModelGatewayHandlers.recordCost wrote to FinOpsLedger — real SQL sums, not numbers made up
// for the page. Each row shows its provider's own cost_model (token_based|compute_based) so a
// compute_based model (no per-token price computed here — see finops.PricingTable's doc) reading
// $0.00 total is honestly distinguishable from a token_based model that's genuinely free/unpriced.
type FinOpsHandlers struct {
	Ledger *store.FinOpsLedger
}

// Register mounts the FinOps routes on mux.
func (h *FinOpsHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /finops/costs", h.showCosts)
}

func (h *FinOpsHandlers) showCosts(w http.ResponseWriter, r *http.Request) {
	totals, err := h.Ledger.TotalsByModel(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Aeon FinOps — cost per model</title></head>
<body>
<h1>Cost per model</h1>
`)

	if len(totals) == 0 {
		fmt.Fprint(w, "<p>no cost events recorded yet</p>\n")
		fmt.Fprint(w, "</body></html>\n")
		return
	}

	var grandTotalUSD float64
	fmt.Fprint(w, "<table><tr><th>provider</th><th>model</th><th>cost_model</th><th>calls</th><th>total cost (USD)</th><th>prompt tokens</th><th>completion tokens</th></tr>\n")
	for _, t := range totals {
		grandTotalUSD += t.TotalCostUSD
		fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%.4f</td><td>%d</td><td>%d</td></tr>\n",
			html.EscapeString(t.Provider), html.EscapeString(t.Model), html.EscapeString(t.CostModel),
			t.CallCount, t.TotalCostUSD, t.TotalPromptTokens, t.TotalCompletionTokens)
	}
	fmt.Fprint(w, "</table>\n")
	fmt.Fprintf(w, "<p>Total: $%.4f</p>\n", grandTotalUSD)
	fmt.Fprint(w, "</body></html>\n")
}
