package api

import (
	"fmt"
	"html"
	"net/http"

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
	var unpricedCalls int64
	fmt.Fprint(w, "<table><tr><th>provider</th><th>model</th><th>cost_model</th><th>calls</th><th>total cost (USD)</th><th>unpriced calls</th><th>prompt tokens</th><th>completion tokens</th></tr>\n")
	for _, t := range totals {
		if t.TotalCostUSD != nil {
			grandTotalUSD += *t.TotalCostUSD
		}
		unpricedCalls += t.UnpricedCalls
		fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td><td>%d</td><td>%d</td><td>%d</td></tr>\n",
			html.EscapeString(t.Provider), html.EscapeString(t.Model), html.EscapeString(orUnknown(t.CostModel)),
			t.CallCount, costCell(t.TotalCostUSD), t.UnpricedCalls, t.TotalPromptTokens, t.TotalCompletionTokens)
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
	fmt.Fprint(w, "</body></html>\n")
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
