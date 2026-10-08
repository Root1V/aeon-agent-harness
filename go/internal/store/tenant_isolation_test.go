package store

import (
	"context"
	"os"
	"testing"
)

// TestOneTenantCannotSeeAnothersData is VRT-AEON-005's acceptance criterion 1, and the negative test
// per table Veritium asked for in place of requiring Row-Level Security.
//
// IT IS A TEST PER TABLE AND NOT PER ROUTE, deliberately. Slice 2 found the Memory Store's isolation
// implemented route by route — four routes read the tenant from the request and five checked none at
// all — and the reason that could happen is that each route was its own chance to forget. These
// assertions are at the store, where the predicate either exists or does not, and the handle cannot
// be built without a tenant.
func TestOneTenantCannotSeeAnothersData(t *testing.T) {
	dsn := os.Getenv("AEON_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("AEON_TEST_PG_DSN not set — skipping Postgres integration test (see make test-go-integration)")
	}
	ctx := context.Background()
	s, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer s.Close()

	a, b := "tenant-a-"+randSuffix(t), "tenant-b-"+randSuffix(t)

	t.Run("agents", func(t *testing.T) {
		name := "iso-agent-" + randSuffix(t)
		manifest := map[string]any{
			"apiVersion": "harness.ai/v1", "kind": "AgentManifest",
			"metadata": map[string]any{"name": name, "version": "0.1.0"},
			"spec": map[string]any{
				"modelPolicy": map[string]any{"profile": "reasoning-balanced"},
				"tools":       map[string]any{}, "runtime": map[string]any{}, "contextPolicy": map[string]any{},
			},
		}
		if _, err := s.AgentRegistryFor(a).Create(ctx, manifest, "owner-a"); err != nil {
			t.Fatalf("create in A: %v", err)
		}

		// NOT FOUND, not forbidden: across the tenant boundary the answer has to be the one a
		// nonexistent record would give (T-7), because a refusal confirms the record is real.
		if _, err := s.AgentRegistryFor(b).Get(ctx, name, "0.1.0"); err == nil {
			t.Error("B read A's agent")
		}
		if containsAgent(t, s.AgentRegistryFor(b), name) {
			t.Error("B's List contains A's agent")
		}
		if !containsAgent(t, s.AgentRegistryFor(a), name) {
			t.Fatal("A cannot see its OWN agent — negative control: the test above proves nothing")
		}

		// THE SAME NAME IN BOTH TENANTS IS TWO AGENTS, which the old PRIMARY KEY (name, version)
		// made impossible: B's create would have collided with A's.
		if _, err := s.AgentRegistryFor(b).Create(ctx, manifest, "owner-b"); err != nil {
			t.Errorf("B cannot register an agent with the same name as A's: %v", err)
		}
	})

	t.Run("tool executions (the same idempotency key is two executions)", func(t *testing.T) {
		// VRT-AEON-005 acceptance criterion 2, verbatim. Before migration 0002 the key WAS the whole
		// primary key, so B's claim found A's row and came back Completed with A's recorded result:
		// a skipped side effect plus somebody else's answer.
		key := "iso-key-" + randSuffix(t)
		args := map[string]any{"path": "/tmp/x"}

		claimA, err := s.ToolExecutionsFor(a).Claim(ctx, key, "artifact.write", "agent@1", args)
		if err != nil {
			t.Fatalf("A claim: %v", err)
		}
		if !claimA.Claimed {
			t.Fatalf("A did not get the claim: %+v", claimA)
		}
		if err := s.ToolExecutionsFor(a).Complete(ctx, key, map[string]any{"who": "A"}); err != nil {
			t.Fatalf("A complete: %v", err)
		}

		claimB, err := s.ToolExecutionsFor(b).Claim(ctx, key, "artifact.write", "agent@1", args)
		if err != nil {
			t.Fatalf("B claim: %v", err)
		}
		if claimB.Completed != nil {
			t.Fatalf("B was handed A's recorded result (%s) — the deduplication table is shared, so "+
				"B's effect never happens and B is told it already did", claimB.Completed)
		}
		if !claimB.Claimed {
			t.Errorf("B neither claimed nor was deduplicated: %+v", claimB)
		}

		// B records ITS OWN result, so the two rows now differ in content as well as in tenant.
		if err := s.ToolExecutionsFor(b).Complete(ctx, key, map[string]any{"who": "B"}); err != nil {
			t.Fatalf("B complete: %v", err)
		}

		// EACH TENANT'S REPEAT GETS ITS OWN RESULT, and this pair is what exercises the predicate on
		// the lookup rather than only the one on the INSERT's conflict target.
		//
		// The first version of this subtest stopped at "B was not handed A's result", and a negative
		// control proved it passed WITHOUT the WHERE tenant_id on the SELECT: B's insert simply does
		// not conflict, so B is claimed and the lookup is never reached. A test that cannot fail when
		// the thing it names is removed is a test that names something else.
		for _, c := range []struct{ tenant, want string }{{a, "A"}, {b, "B"}} {
			again, err := s.ToolExecutionsFor(c.tenant).Claim(ctx, key, "artifact.write", "agent@1", args)
			if err != nil {
				t.Fatalf("re-claim in %s: %v", c.tenant, err)
			}
			if again.Completed == nil {
				t.Errorf("%s's repeat was not deduplicated — tenancy broke the thing it was scoping", c.tenant)
				continue
			}
			if got := string(again.Completed); !containsJSONValue(got, c.want) {
				t.Errorf("%s's repeat returned %s, want the result %s recorded — the lookup is not "+
					"scoped, so a repeat can be answered with another tenant's result", c.tenant, got, c.want)
			}
		}
	})

	t.Run("costs", func(t *testing.T) {
		// Criterion 4: GET /finops/costs sums separately. Asserted at the store, which is where the
		// sum is computed; the handler's job is only to pass the caller's tenant.
		model := "iso-model-" + randSuffix(t)
		cm := "token_based"
		cost := 1.25
		if err := s.FinOpsLedgerFor(a).Record(ctx, CostEntry{
			Provider: "iso-provider", Model: model, CostModel: &cm, CostUSD: &cost,
		}); err != nil {
			t.Fatalf("record in A: %v", err)
		}

		if totalFor(t, s.FinOpsLedgerFor(b), model) != 0 {
			t.Error("B's dashboard includes A's spend — cost per case is a business and audit fact")
		}
		if got := totalFor(t, s.FinOpsLedgerFor(a), model); got != cost {
			t.Fatalf("A's own total = %v, want %v — negative control", got, cost)
		}
	})

	t.Run("remote agents", func(t *testing.T) {
		id := "iso-remote-" + randSuffix(t)
		if _, err := s.RemoteAgentsFor(a).Declare(ctx, RemoteAgentRecord{
			AgentID: id, URL: "http://example.invalid", Risk: RiskLow,
		}); err != nil {
			t.Fatalf("declare in A: %v", err)
		}
		if _, err := s.RemoteAgentsFor(b).Get(ctx, id); err == nil {
			t.Error("B read A's declared destination — delegation targets are a tenant's own statement")
		}
		if _, err := s.RemoteAgentsFor(a).Get(ctx, id); err != nil {
			t.Fatalf("A cannot read its own: %v — negative control", err)
		}
	})
}

func containsAgent(t *testing.T, r *AgentRegistry, name string) bool {
	t.Helper()
	recs, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, rec := range recs {
		if rec.Name == name {
			return true
		}
	}
	return false
}

func totalFor(t *testing.T, l *FinOpsLedger, model string) float64 {
	t.Helper()
	totals, err := l.TotalsByModel(context.Background())
	if err != nil {
		t.Fatalf("TotalsByModel: %v", err)
	}
	for _, tot := range totals {
		if tot.Model == model && tot.TotalCostUSD != nil {
			return *tot.TotalCostUSD
		}
	}
	return 0
}

// containsJSONValue is a deliberately crude check: the point is which tenant's recorded result came
// back, and a JSON decode here would add a dependency without adding an assertion.
func containsJSONValue(body, want string) bool {
	return len(body) > 0 && (body == `{"who":"`+want+`"}` || body == `{"who": "`+want+`"}`)
}
