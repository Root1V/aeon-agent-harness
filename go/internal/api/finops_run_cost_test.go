package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/checkpoint"
	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// FIN-002, Veritium's criterion verbatim (`VRT-AEON-007`, point 2): *«un run con llamadas devuelve la
// suma que hoy muestra la tabla HTML, y el run de otro tenant responde 404 (vuestro T-7)»*.
//
// THE FIRST HALF IS ASSERTED AGAINST THE HTML ITSELF and not against numbers written twice. "The same
// sum" is the whole value of this route — a consumer that reads JSON and an operator that reads the
// dashboard must not be able to disagree about what a case cost — and the only way to measure that is
// to parse the page and compare. Two independent constants would pass while the two surfaces drifted.
func TestOneRunsCostIsAvailableAsData(t *testing.T) {
	ctx := context.Background()
	s := newAPITestStore(t)

	tokenModel := "fin002-token-" + randSuffix(t)
	computeModel := "fin002-compute-" + randSuffix(t)
	pricing := finops.NewPricingTable([]finops.Rate{
		{Provider: "fake-token", Model: tokenModel, CostModel: "token_based", InputPerMillionUSD: 10, OutputPerMillionUSD: 30},
		{Provider: "fake-compute", Model: computeModel, CostModel: "compute_based"},
	})
	gw := modelgateway.New()
	gw.RegisterProvider("fake-token", &finOpsFakeProvider{costModel: "token_based"})
	gw.RegisterProvider("fake-compute", &finOpsFakeProvider{costModel: "compute_based"})

	newServer := func(tenant string) *httptest.Server {
		mux := http.NewServeMux()
		(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: s}).Register(mux)
		(&FinOpsHandlers{Ledger: s}).Register(mux)
		(&CheckpointHandlers{Store: s}).Register(mux)
		srv := httptest.NewServer(authWrapTenant(t, mux, tenant))
		t.Cleanup(srv.Close)
		return srv
	}
	srv := newServer("default")

	runID := "fin002-run-" + randSuffix(t)
	// Two priced calls: 1000 prompt + 500 completion each, at $10/$30 per million = $0.025 a call.
	for i := 0; i < 2; i++ {
		status, body := postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: "fake-token", Model: tokenModel, Priority: 0}},
			RenderedContext: map[string]any{"messages": []any{}},
			RunID:           runID,
		})
		if status != http.StatusOK {
			t.Fatalf("POST /decide %d: status=%d body=%v", i, status, body)
		}
	}

	got := getRunCost(t, srv, runID, http.StatusOK)
	if got.ModelCalls != 2 || got.PricedCalls != 2 || got.UnpricedCalls != 0 {
		t.Errorf("calls = %d (%d priced, %d unpriced), want 2/2/0", got.ModelCalls, got.PricedCalls, got.UnpricedCalls)
	}
	if got.CostUSD == nil || !closeEnough(*got.CostUSD, 0.05) {
		t.Errorf("cost_usd = %v, want 0.05", got.CostUSD)
	}
	if got.PromptTokens != 2000 || got.CompletionTokens != 1000 || got.Tokens != 3000 {
		t.Errorf("tokens = %d in / %d out / %d total, want 2000/1000/3000 — asked for separately because "+
			"input and output are priced differently", got.PromptTokens, got.CompletionTokens, got.Tokens)
	}
	if got.DistinctModels != 1 {
		t.Errorf("distinct_models = %d, want 1", got.DistinctModels)
	}

	// AND THE SAME NUMBERS THE DASHBOARD PRINTS, read off the page — every column it has, not just
	// the cost. Two surfaces over one ledger disagreeing about what a case cost is worse than having
	// only one of them.
	row := runRowFromDashboard(t, fetchDashboard(t, srv), runID)
	if row.calls != got.ModelCalls || row.models != got.DistinctModels || row.unpriced != got.UnpricedCalls ||
		row.prompt != got.PromptTokens || row.completion != got.CompletionTokens {
		t.Errorf("the HTML row says calls=%d models=%d unpriced=%d tokens=%d/%d and the JSON says "+
			"%d/%d/%d/%d/%d", row.calls, row.models, row.unpriced, row.prompt, row.completion,
			got.ModelCalls, got.DistinctModels, got.UnpricedCalls, got.PromptTokens, got.CompletionTokens)
	}
	if got.CostUSD == nil || !closeEnough(*got.CostUSD, row.costUSD) {
		t.Errorf("the HTML says $%.4f for this run and the JSON says %v", row.costUSD, jsonCost(got.CostUSD))
	}

	t.Run("another tenant's run is 404 and says nothing about it existing", func(t *testing.T) {
		// T-7. Both the ledger and the journal are scoped to the caller, so to this caller the run does
		// not exist — and the answer carries no hint that it exists anywhere else.
		other := newServer("tenant-b-" + randSuffix(t))
		body := getRunCostRaw(t, other, runID, http.StatusNotFound)
		if strings.Contains(string(body), "tenant") && strings.Contains(string(body), "default") {
			t.Errorf("the 404 body names the owning tenant: %s", body)
		}
	})

	t.Run("an unknown run id is 404", func(t *testing.T) {
		getRunCostRaw(t, srv, "fin002-never-existed-"+randSuffix(t), http.StatusNotFound)
	})

	t.Run("a run that exists and spent nothing is 200 with zeros, not 404", func(t *testing.T) {
		// THE DISTINCTION THIS ROUTE EXISTS TO KEEP. A run refused by policy or by its budget before its
		// first model call leaves no cost row — the same silence as a typo. Reporting it as "no such run"
		// would hide exactly the run an operator is looking for.
		quiet := "fin002-quiet-run-" + randSuffix(t)
		appendCheckpoint(t, srv, quiet)

		got := getRunCost(t, srv, quiet, http.StatusOK)
		if got.ModelCalls != 0 {
			t.Fatalf("model_calls = %d, want 0", got.ModelCalls)
		}
		if got.CostUSD != nil {
			t.Errorf("cost_usd = %v, want null — a run that spent nothing has no price, and $0.00 would "+
				"read as 'priced at zero', which is a different fact", *got.CostUSD)
		}
	})

	t.Run("a run whose every call is unpriced reports null and says how many", func(t *testing.T) {
		computeRun := "fin002-compute-run-" + randSuffix(t)
		status, body := postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: "fake-compute", Model: computeModel, Priority: 0}},
			RenderedContext: map[string]any{"messages": []any{}},
			RunID:           computeRun,
		})
		if status != http.StatusOK {
			t.Fatalf("POST /decide (compute_based): status=%d body=%v", status, body)
		}
		got := getRunCost(t, srv, computeRun, http.StatusOK)
		if got.CostUSD != nil {
			t.Errorf("cost_usd = %v, want null for a run billed by GPU-seconds", *got.CostUSD)
		}
		if got.ModelCalls != 1 || got.UnpricedCalls != 1 || got.PricedCalls != 0 {
			t.Errorf("calls = %d (%d priced, %d unpriced), want 1/0/1 — a consumer showing this number "+
				"has to be able to say the figure is incomplete", got.ModelCalls, got.PricedCalls, got.UnpricedCalls)
		}
		// And the tokens are still there: unpriced is not unmeasured.
		if got.PromptTokens != 1000 || got.CompletionTokens != 500 {
			t.Errorf("tokens = %d/%d, want 1000/500: no price is a different fact from no usage",
				got.PromptTokens, got.CompletionTokens)
		}
	})

	// The ceiling reads the same rows, and this is the assertion that keeps the new columns honest
	// against the old ones rather than against a constant.
	spend, err := s.FinOpsLedgerFor("default").SpendForRun(ctx, runID)
	if err != nil {
		t.Fatalf("SpendForRun: %v", err)
	}
	if spend.PromptTokens+spend.CompletionTokens != spend.Tokens {
		t.Errorf("prompt %d + completion %d != tokens %d, so the budget ceiling and this route are "+
			"summing different things", spend.PromptTokens, spend.CompletionTokens, spend.Tokens)
	}
}

func closeEnough(got, want float64) bool { return got-want < 1e-9 && want-got < 1e-9 }

func getRunCost(t *testing.T, srv *httptest.Server, runID string, wantStatus int) runCostResponse {
	t.Helper()
	var parsed runCostResponse
	if err := json.Unmarshal(getRunCostRaw(t, srv, runID, wantStatus), &parsed); err != nil {
		t.Fatalf("decoding the run cost: %v", err)
	}
	return parsed
}

func getRunCostRaw(t *testing.T, srv *httptest.Server, runID string, wantStatus int) []byte {
	t.Helper()
	resp := getAuthed(t, srv.URL+"/finops/runs/"+runID)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the run cost: %v", err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET /finops/runs/%s = %d, want %d: %s", runID, resp.StatusCode, wantStatus, raw)
	}
	return raw
}

func fetchDashboard(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp := getAuthed(t, srv.URL+"/finops/costs")
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the dashboard: %v", err)
	}
	return string(raw)
}

// runRowFromDashboard pulls one run's call count and cost out of the rendered page.
//
// Parsing HTML in a test is ugly and it is the point: the page is what an operator reads, so the only
// honest comparison is against the bytes it serves. The alternative — asserting the JSON against a
// constant — would stay green while the two surfaces drifted apart, which is the single failure this
// route must not have.
func runRowFromDashboard(t *testing.T, page, runID string) dashboardRunRow {
	t.Helper()
	// The page's own column order: key, calls, models, cost, unpriced calls, prompt, completion.
	cells := regexp.MustCompile(`<tr><td>` + regexp.QuoteMeta(runID) +
		`</td><td>(\d+)</td><td>(\d+)</td><td>\$?([0-9.]+)</td><td>(\d+)</td><td>(\d+)</td><td>(\d+)</td>`).
		FindStringSubmatch(page)
	if cells == nil {
		t.Fatalf("the dashboard has no row for run %s, so there is nothing to compare against", runID)
	}
	num := func(i int) int64 {
		n, err := strconv.ParseInt(cells[i], 10, 64)
		if err != nil {
			t.Fatalf("cell %d (%q) of the dashboard row: %v", i, cells[i], err)
		}
		return n
	}
	cost, err := strconv.ParseFloat(cells[3], 64)
	if err != nil {
		t.Fatalf("cost %q in the dashboard row: %v", cells[3], err)
	}
	return dashboardRunRow{
		calls: num(1), models: num(2), costUSD: cost,
		unpriced: num(4), prompt: num(5), completion: num(6),
	}
}

type dashboardRunRow struct {
	calls, models, unpriced, prompt, completion int64
	costUSD                                     float64
}

// jsonCost prints the pointer's VALUE, or the word that distinguishes "no price" from zero.
func jsonCost(c *float64) string {
	if c == nil {
		return "null"
	}
	return strconv.FormatFloat(*c, 'f', 4, 64)
}

// appendCheckpoint makes a run exist without spending anything, through the real route.
func appendCheckpoint(t *testing.T, srv *httptest.Server, runID string) {
	t.Helper()
	body, _ := json.Marshal(appendCheckpointRequest{
		StepID: "s1", Phase: string(checkpoint.PhaseAttempted),
		Payload: json.RawMessage(`{"ok":true}`),
	})
	resp := postJSONAuthed(t, srv.URL+"/runs/"+runID+"/checkpoints", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("making run %s exist: status=%d", runID, resp.StatusCode)
	}
}

var _ = store.RunSpend{}
