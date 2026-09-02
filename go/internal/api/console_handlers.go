package api

import (
	"fmt"
	"html"
	"net/http"

	"github.com/aeon-ai/aeon/go/internal/runcontroller"
	"github.com/aeon-ai/aeon/go/internal/tempoclient"
)

// ConsoleHandlers is OBS-002's Agent Console: a real, server-rendered HTML page for one run,
// combining the same two real data sources aeon trace/aeon status already read — the Run
// Controller (Temporal, via Controller.Status) and Tempo (via tempoclient, OBS-001's real spans) —
// so a run can be inspected in a browser, not just the CLI.
//
// Scope, real and deliberately bounded (see backlog.md): this is the "trace explorer" third of
// OBS-002's three-part vision. The "context inspector" and "evidence graph" are not built here,
// because neither has a durable, cross-process-queryable real data source to inspect yet —
// aeon_context's lane state lives only in a run's own Python process while it executes, and
// aeon_evidence.EvidenceLedger is a plain in-memory Python object, never persisted anywhere a
// browser (or anything else) could query after the fact. Building a real UI on top of fake/sample
// context or evidence data would violate the same "no mocks" standard every other feature in this
// project has held to — so those two panes wait on real persistence for their own data first.
type ConsoleHandlers struct {
	Controller *runcontroller.Controller
	TempoURL   string // e.g. "http://tempo:3200"; empty disables the trace panel (Run status still renders)
}

// Register mounts the console route on mux.
func (h *ConsoleHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /console/runs/{run_id}", h.showRun)
}

func (h *ConsoleHandlers) showRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	ctx := r.Context()

	status, err := h.Controller.Status(ctx, workflowID(runID))
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("run %q: %w", runID, err))
		return
	}

	var traces []tempoclient.Trace
	var traceErr error
	if h.TempoURL != "" {
		traces, traceErr = tempoclient.SearchByAgentName(ctx, h.TempoURL, runID)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Aeon Agent Console — %s</title></head>
<body>
<h1>Run %s</h1>
<table>
<tr><th>Status</th><td>%s</td></tr>
<tr><th>Paused</th><td>%v</td></tr>
</table>
`, html.EscapeString(runID), html.EscapeString(runID), html.EscapeString(status.Status), status.Paused)

	if status.BudgetsConsumed != nil {
		fmt.Fprintf(w, "<h2>Budgets consumed</h2><ul>\n")
		for k, v := range status.BudgetsConsumed {
			fmt.Fprintf(w, "<li>%s: %v</li>\n", html.EscapeString(k), v)
		}
		fmt.Fprintf(w, "</ul>\n")
	}

	fmt.Fprintf(w, "<h2>Trace</h2>\n")
	switch {
	case h.TempoURL == "":
		fmt.Fprintf(w, "<p>tracing not configured</p>\n")
	case traceErr != nil:
		fmt.Fprintf(w, "<p>error querying Tempo: %s</p>\n", html.EscapeString(traceErr.Error()))
	case len(traces) == 0:
		fmt.Fprintf(w, "<p>no traces found for this run yet</p>\n")
	default:
		fmt.Fprintf(w, "<table><tr><th>trace_id</th><th>root service</th><th>root span</th><th>duration</th></tr>\n")
		for _, t := range traces {
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%dms</td></tr>\n",
				html.EscapeString(t.TraceID), html.EscapeString(t.RootServiceName), html.EscapeString(t.RootTraceName), t.DurationMs)
		}
		fmt.Fprintf(w, "</table>\n")
	}

	fmt.Fprintf(w, "</body></html>\n")
}
