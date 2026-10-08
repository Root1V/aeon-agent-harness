package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// TestCostIsAttributedToRunAndAgent is OBS-003b's acceptance test.
//
// WHAT WAS MISSING, and it is the shape this project keeps finding: both ends were built and the wire
// between them was never run. `decideRequest` has accepted `run_id`/`agent_manifest_ref` since
// OBS-003 and `FinOpsLedger.Record` has written them to nullable columns since then — and no Python
// caller ever filled either. So `GET /finops/costs` aggregated cost per MODEL correctly, and the
// other half of OBS-003's own title in the spec, cost per run and per agent, had no data to render.
// Nothing failed. The columns were there, the rows were there, and every one of them said NULL.
//
// THE PART THAT NEEDED DESIGNING IS THE NULL GROUP. A call made outside any run is legitimate — a
// script hitting /decide, an eval harness — so those rows exist and their cost is real. Dropping them
// from a per-run page would make its total quietly smaller than the per-model page's over the same
// ledger, with nothing anywhere saying why, and a reader comparing the two would take the smaller
// number as the true one. So the group is REPORTED, and the invariant below is asserted directly:
// summing by run and summing by model over the same table give the same money.
//
// Real Postgres, the real /decide handler, the real ledger and the real dashboard HTML — the property
// is about what lands in the table and what the aggregation does with it, and a fake ledger would
// assert the shape of a call and nothing about either.
func TestCostIsAttributedToRunAndAgent(t *testing.T) {
	ctx := context.Background()
	s := newAPITestStore(t)
	ledgerStore := s
	ledger := s.FinOpsLedgerFor("default")

	// Own model name per test run: these aggregations are global, and a fixed name would make the
	// assertions depend on how many times the suite had been run against this database before.
	model := "attrib-test-model-" + randSuffix(t)
	runA := "run-a-" + randSuffix(t)
	runB := "run-b-" + randSuffix(t)
	agentA := "attrib-agent-a@0.1.0-" + randSuffix(t)

	pricing := finops.NewPricingTable([]finops.Rate{
		{Provider: "fake-token", Model: model, CostModel: "token_based", InputPerMillionUSD: 10, OutputPerMillionUSD: 30},
	})
	gw := modelgateway.New()
	gw.RegisterProvider("fake-token", &finOpsFakeProvider{costModel: "token_based"})

	mux := http.NewServeMux()
	(&ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledgerStore}).Register(mux)
	(&FinOpsHandlers{Ledger: ledgerStore}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)

	// 1000 prompt + 500 completion at $10/$30 per million = $0.025 per call.
	const perCall = 0.025

	decide := func(t *testing.T, runID, agentRef string) {
		t.Helper()
		status, body := postDecide(t, srv, decideRequest{
			Candidates:       []decideCandidate{{Provider: "fake-token", Model: model, Priority: 0}},
			RenderedContext:  map[string]any{"messages": []any{}},
			RunID:            runID,
			AgentManifestRef: agentRef,
		})
		if status != http.StatusOK {
			t.Fatalf("POST /decide: status=%d body=%v", status, body)
		}
	}

	// Three calls, chosen so the two null groups cannot be the same set:
	//   runA + agentA   fully attributed
	//   runB, no agent  a run that identified itself but not which agent it runs as
	//   neither         a call from outside any run
	// The middle one is not hypothetical: it is exactly what every Deep Research run looked like
	// before MDL-015 added agent_manifest_ref, and what an interop run still looks like today.
	decide(t, runA, agentA)
	decide(t, runA, agentA)
	decide(t, runB, "")
	decide(t, "", "")

	find := func(totals []store.AttributionTotal, key string) *store.AttributionTotal {
		for i := range totals {
			if totals[i].Key != nil && *totals[i].Key == key {
				return &totals[i]
			}
		}
		return nil
	}
	nullGroup := func(totals []store.AttributionTotal) *store.AttributionTotal {
		for i := range totals {
			if totals[i].Key == nil {
				return &totals[i]
			}
		}
		return nil
	}

	byRun, err := ledger.TotalsByRun(ctx)
	if err != nil {
		t.Fatalf("TotalsByRun: %v", err)
	}
	byAgent, err := ledger.TotalsByAgent(ctx)
	if err != nil {
		t.Fatalf("TotalsByAgent: %v", err)
	}

	t.Run("a run's cost is the sum of its own calls", func(t *testing.T) {
		got := find(byRun, runA)
		if got == nil {
			t.Fatalf("run %q is not in TotalsByRun — this is the question OBS-003 named and could not answer: %v", runA, byRun)
		}
		if got.CallCount != 2 {
			t.Fatalf("call_count = %d, want 2", got.CallCount)
		}
		if got.TotalCostUSD == nil {
			t.Fatalf("total_cost_usd is nil for a run whose calls were all priced")
		}
		if diff := *got.TotalCostUSD - 2*perCall; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("total_cost_usd = %v, want %v", *got.TotalCostUSD, 2*perCall)
		}
		if got.DistinctModels != 1 {
			t.Fatalf("distinct_models = %d, want 1", got.DistinctModels)
		}
	})

	t.Run("an agent's cost is the sum across its runs", func(t *testing.T) {
		got := find(byAgent, agentA)
		if got == nil {
			t.Fatalf("agent %q is not in TotalsByAgent: %v", agentA, byAgent)
		}
		if got.CallCount != 2 {
			t.Fatalf("call_count = %d, want 2", got.CallCount)
		}
	})

	t.Run("naming a run is not the same as naming an agent", func(t *testing.T) {
		// runB said which run it was and not which agent. It must appear as a named run AND inside the
		// agent-side null group — if the two nulls were one set, "which of our agents costs most"
		// would silently exclude every run that predates MDL-015 while looking complete.
		if find(byRun, runB) == nil {
			t.Fatalf("run %q is missing from TotalsByRun: %v", runB, byRun)
		}
		agentNull := nullGroup(byAgent)
		if agentNull == nil {
			t.Fatalf("TotalsByAgent has no null group, so the calls that named no agent were dropped: %v", byAgent)
		}
		runNull := nullGroup(byRun)
		if runNull == nil {
			t.Fatalf("TotalsByRun has no null group, so the calls that named no run were dropped: %v", byRun)
		}
		if agentNull.CallCount <= runNull.CallCount {
			t.Fatalf("the agent-side null group (%d calls) is not larger than the run-side one (%d): runB named a "+
				"run and no agent, so it must be inside the first and outside the second",
				agentNull.CallCount, runNull.CallCount)
		}
	})

	t.Run("no money disappears between the three aggregations", func(t *testing.T) {
		// The invariant that makes the null group safe to report, asserted over the WHOLE table rather
		// than over this test's rows — it holds regardless of what else the database contains, which is
		// what makes it worth asserting at all.
		byModel, err := ledger.TotalsByModel(ctx)
		if err != nil {
			t.Fatalf("TotalsByModel: %v", err)
		}
		sum := func(name string, cost func() (float64, int64)) (float64, int64) {
			usd, calls := cost()
			t.Logf("%s: $%.6f over %d calls", name, usd, calls)
			return usd, calls
		}
		modelUSD, modelCalls := sum("by model", func() (float64, int64) {
			var usd float64
			var calls int64
			for _, m := range byModel {
				if m.TotalCostUSD != nil {
					usd += *m.TotalCostUSD
				}
				calls += m.CallCount
			}
			return usd, calls
		})
		for _, tc := range []struct {
			name   string
			totals []store.AttributionTotal
		}{{"by run", byRun}, {"by agent", byAgent}} {
			usd, calls := sum(tc.name, func() (float64, int64) {
				var usd float64
				var calls int64
				for _, a := range tc.totals {
					if a.TotalCostUSD != nil {
						usd += *a.TotalCostUSD
					}
					calls += a.CallCount
				}
				return usd, calls
			})
			if calls != modelCalls {
				t.Fatalf("%s counts %d calls, by model counts %d — one aggregation is losing rows", tc.name, calls, modelCalls)
			}
			if diff := usd - modelUSD; diff > 1e-6 || diff < -1e-6 {
				t.Fatalf("%s totals $%v, by model totals $%v — the difference is money attributed to nobody and shown to nobody", tc.name, usd, modelUSD)
			}
		}
	})

	t.Run("the dashboard shows both attributions and names what is unattributed", func(t *testing.T) {
		resp := getAuthed(t, srv.URL+"/finops/costs")
		defer resp.Body.Close()
		// io.ReadAll and not a single Read into a fixed buffer: this page grows with the shared
		// database, and a fixed buffer made an earlier version of this assertion fail once the page
		// outgrew one read.
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading the dashboard: %v", err)
		}
		page := string(raw)
		for _, want := range []string{"Cost per run", "Cost per agent", runA, runB, agentA} {
			if !strings.Contains(page, want) {
				t.Fatalf("the dashboard does not mention %q", want)
			}
		}
		if !strings.Contains(page, "named no run") {
			t.Fatalf("the dashboard does not say that some calls named no run, so every figure on it reads as complete when it is not")
		}
		if !strings.Contains(page, "named no agent") {
			t.Fatalf("the dashboard does not say that some calls named no agent")
		}
	})
}
