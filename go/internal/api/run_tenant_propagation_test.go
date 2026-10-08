package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/auth"
	"github.com/aeon-ai/aeon/go/internal/checkpoint"
	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/httpserver"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// VRT-AEON-005's reopened criterion 3, with Veritium's acceptance criteria adopted verbatim and the
// two surfaces we found while verifying their finding.
//
// WHAT THEY MEASURED, on their own deployment against d73da2b: policy is evaluated against
// `caller.Tenant`, and for a step of a run the caller is the AEON WORKER — one token, therefore one
// tenant. So tenant B's permit was never consulted for B's runs, and the obvious workaround (copying
// B's permit into the worker's bundle) makes it apply to EVERY tenant's runs, which is criterion 3
// inverted.
//
// WHAT WE FOUND VERIFYING IT: six surfaces, not one. The two in this file's later subtests are the
// ones with consequences beyond attribution — the cost ledger, and the agent registry the COST
// CEILING is read from. The ceiling is the worst of the six and the only one with NO SYMPTOM:
// `overBudget` answers a missing manifest by logging it and allowing the call (MDL-017's deliberate
// choice), so a run whose agent is in another tenant is not capped at all. Nothing fails.
//
// Real Postgres and the real handlers, because every one of these properties is about which rows
// land where.
func TestAStepOfARunIsJudgedAndBilledAgainstTheRunsTenant(t *testing.T) {
	ctx := context.Background()
	const (
		workerTenant = "ops"
		tenantA      = "tenant-a"
		tenantB      = "tenant-b"
		activityName = "veritium.case.start_run"
		queueA       = "a-online"
		queueB       = "b-online"
	)
	agentRef := "propagation-agent@0.1.0"

	// Veritium's fixture: two bundles, one permit each, for the SAME activity on DIFFERENT queues.
	// The queue is what makes the two permits distinguishable, so "allowed" can only come from the
	// right bundle — a test where both permits matched would pass against the defect.
	policySet := twoTenantActivityPolicySet(t, agentRef, activityName, map[string]string{
		tenantA: queueA,
		tenantB: queueB,
	})

	t.Run("a run submitted by B executes the activity B permits, and A's same graph is denied", func(t *testing.T) {
		mux := http.NewServeMux()
		(&ToolGatewayHandlers{Policy: policySet, Executor: toolexec.NewExecutor()}).Register(mux)
		srv := httptest.NewServer(workerAuthWrap(t, mux, workerTenant, []string{tenantA, tenantB}, agentRef))
		t.Cleanup(srv.Close)

		// Veritium's first criterion. The caller is the worker, in `ops`, which has no bundle at all —
		// so before the propagation this answered 403 "no policy bundle is loaded for this tenant".
		if allowed, body := checkActivityPolicy(t, srv, tenantB, agentRef, activityName, queueB); !allowed {
			t.Fatalf("B's run was not allowed to run the activity B permits: %v", body)
		}

		// Veritium's second criterion, and the half that makes the first mean something: the SAME
		// graph, submitted by A, must be denied — A's bundle permits this activity only on its own
		// queue. Without it the test would pass against a gateway that allows everything.
		if allowed, body := checkActivityPolicy(t, srv, tenantA, agentRef, activityName, queueB); allowed {
			t.Fatalf("A's run was allowed to run the activity only B permits: %v", body)
		}

		// And the reverse, so neither bundle is simply permissive.
		if allowed, _ := checkActivityPolicy(t, srv, tenantA, agentRef, activityName, queueA); !allowed {
			t.Error("A's run was denied the activity A permits — this bundle authorizes nothing and the " +
				"subtest above proves nothing")
		}

		// THE NEGATIVE CONTROL Veritium asked for, run here rather than described: with no header the
		// tenant is the worker's own, `ops` has no bundle, and the first case falls.
		if allowed, body := checkActivityPolicy(t, srv, "", agentRef, activityName, queueB); allowed {
			t.Fatalf("with no run tenant the call was still allowed, so this test is not measuring the "+
				"propagation at all: %v", body)
		}
	})

	t.Run("a caller may not name a tenant the operator did not list", func(t *testing.T) {
		// The privilege is a list and not a flag. A worker entitled to A and B must not be able to
		// speak for C, and the refusal has to SAY so — a silently ignored header would leave an
		// operator debugging a worker whose tenant is wrong for no stated reason.
		mux := http.NewServeMux()
		(&ToolGatewayHandlers{Policy: policySet, Executor: toolexec.NewExecutor()}).Register(mux)
		srv := httptest.NewServer(workerAuthWrap(t, mux, workerTenant, []string{tenantA}, agentRef))
		t.Cleanup(srv.Close)

		status, body := postActivityPolicy(t, srv, tenantB, agentRef, activityName, queueB)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 for a tenant this caller was not entitled to: %v", status, body)
		}
		// THE MESSAGE AND NOT JUST THE STATUS, because the status alone cannot tell the two refusals
		// apart and the first version of this subtest could not either. Found by the negative control:
		// with the header ignored the tenant becomes the worker's own, `ops` has no bundle, and
		// engineFor answers 403 "no policy bundle is loaded for this tenant" — so the subtest passed
		// against the very defect it is meant to catch. A 403 for the wrong reason is still a 403.
		msg, _ := body["error"].(string)
		if !strings.Contains(msg, "may not act for tenant") {
			t.Fatalf("the refusal does not say the caller was not entitled to this tenant, so it is "+
				"indistinguishable from a missing bundle: %q", msg)
		}
		if !strings.Contains(msg, tenantB) {
			t.Errorf("the refusal does not name the tenant that was refused: %q", msg)
		}
	})

	t.Run("the cost row and the CEILING both come from the run's tenant", func(t *testing.T) {
		// Veritium's third criterion ("the cost rows of B's run stay in B") plus ours: the ceiling.
		//
		// The ceiling is the one that had no symptom. The agent and its budget live in B, where
		// POST /runs put them; with the worker as caller the gateway looked in `ops`, found no
		// manifest, logged it and let the call through. Measured before this change: a 1-token
		// ceiling answered 200.
		s := newAPITestStore(t)
		agentsB := s.AgentRegistryFor(tenantB)
		model := "propagation-" + randSuffix(t)
		agentName := "propagation-agent-" + randSuffix(t)
		ref := agentName + "@0.1.0"
		if _, err := agentsB.Create(ctx, map[string]any{
			"apiVersion": "harness.ai/v1", "kind": "Agent",
			"metadata": map[string]any{"name": agentName, "version": "0.1.0", "owner": "test", "lifecycle": "Draft"},
			// 1500 tokens per call (finOpsFakeProvider), ceiling 1500: the first call is allowed and
			// the second is refused.
			"spec": map[string]any{"runtime": map[string]any{"budgets": map[string]any{"tokens": 1500}}},
		}, "test"); err != nil {
			t.Fatalf("registering the agent in %s: %v", tenantB, err)
		}

		gw := modelgateway.New()
		gw.RegisterProvider("fake-compute", &finOpsFakeProvider{costModel: "compute_based"})
		mux := http.NewServeMux()
		(&ModelGatewayHandlers{
			Gateway: gw,
			Pricing: finops.NewPricingTable([]finops.Rate{{Provider: "fake-compute", Model: model, CostModel: "compute_based"}}),
			Ledger:  s, Agents: s,
		}).Register(mux)
		srv := httptest.NewServer(workerAuthWrap(t, mux, workerTenant, []string{tenantA, tenantB}, ref))
		t.Cleanup(srv.Close)

		runID := "propagation-run-" + randSuffix(t)
		call := func() (int, map[string]any) {
			return postDecideAs(t, srv, tenantB, decideRequest{
				Candidates:       []decideCandidate{{Provider: "fake-compute", Model: model, Priority: 0}},
				RenderedContext:  map[string]any{"messages": []any{}},
				RunID:            runID,
				AgentManifestRef: ref,
			})
		}
		if status, body := call(); status != http.StatusOK {
			t.Fatalf("the first call was refused: status=%d body=%v", status, body)
		}
		if status, body := call(); status != http.StatusPaymentRequired {
			t.Fatalf("the second call returned %d, want 402. The ceiling lives in %s and the caller is in "+
				"%s, so this is the subtest that proves the registry is read for the RUN's tenant: %v",
				status, tenantB, workerTenant, body)
		}

		// Veritium's third criterion: the rows are in B.
		spendB, err := s.FinOpsLedgerFor(tenantB).SpendForRun(ctx, runID)
		if err != nil {
			t.Fatalf("SpendForRun in %s: %v", tenantB, err)
		}
		if spendB.Tokens != 1500 || spendB.ModelCalls != 1 {
			t.Fatalf("%s recorded %d tokens over %d calls, want 1500 over 1 — the refused call must leave "+
				"no row", tenantB, spendB.Tokens, spendB.ModelCalls)
		}
		// And NOT in the worker's tenant, which is where every one of them used to land.
		spendOps, err := s.FinOpsLedgerFor(workerTenant).SpendForRun(ctx, runID)
		if err != nil {
			t.Fatalf("SpendForRun in %s: %v", workerTenant, err)
		}
		if spendOps.ModelCalls != 0 {
			t.Fatalf("%s recorded %d calls for a run belonging to %s — the attribution defect is still "+
				"there, just pointing somewhere else", workerTenant, spendOps.ModelCalls, tenantB)
		}
	})

	t.Run("a denied step's journal entry lands in the run's tenant, not the worker's", func(t *testing.T) {
		// OUR SECOND ADDITION to Veritium's criteria. A policy denial is journalled as a completed step
		// with an explicit denied outcome (INT-011), and that write used the caller's tenant — so a
		// run's journal had a hole in whichever tenant submitted it and a stray entry in the worker's.
		// It is what links this finding to Veritium's own VRT-AEON-004.
		s := newAPITestStore(t)

		// Neither bundle permits any TOOL, only the external activity above, so /execute is denied by
		// default-deny — which is the path that journals. An executor that fails the test if reached,
		// for the reason denied_outcome_test.go gives: a journal entry describing a call that actually
		// happened is worse than no entry.
		executor := toolexec.NewExecutor()
		executor.Register("shell.exec", func(string, map[string]any) (map[string]any, error) {
			t.Error("the executor ran a tool the policy denied — the journal record would be a lie")
			return map[string]any{}, nil
		})

		mux := http.NewServeMux()
		(&ToolGatewayHandlers{Policy: policySet, Executor: executor, Checkpointer: s, Executions: s}).Register(mux)
		srv := httptest.NewServer(workerAuthWrap(t, mux, workerTenant, []string{tenantA, tenantB}, agentRef))
		t.Cleanup(srv.Close)

		runID := "journal-run-" + randSuffix(t)
		stepID := "step-1"
		raw, _ := json.Marshal(toolCallRequest{
			AgentManifestRef: agentRef, ToolName: "shell.exec",
			Args: map[string]any{"cmd": "true"}, RunID: runID, StepID: stepID,
		})
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/execute", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(RunTenantHeader, tenantB)
		resp, err := http.DefaultClient.Do(authorize(req))
		if err != nil {
			t.Fatalf("POST /execute: %v", err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 for a tool neither bundle permits: %v", resp.StatusCode, body)
		}

		inB, err := s.CheckpointerFor(tenantB).Load(ctx, runID)
		if err != nil {
			t.Fatalf("Load from %s: %v", tenantB, err)
		}
		// Asserted through the OUTCOME and not just a count: a record exists is weaker than a record
		// that says the step was denied, and INT-011's whole point is that "denied" and "nobody knows"
		// must not be the same entry.
		outcome, _, found := inB.StepOutcome("", stepID)
		if !found {
			t.Fatalf("%s holds no outcome for step %q (it has %d records) — the denial was journalled "+
				"in another tenant, so the run has a hole in its journal where it was submitted",
				tenantB, stepID, len(inB.Records()))
		}
		if outcome != checkpoint.OutcomeDeniedByPolicy {
			t.Errorf("the recorded outcome is %q, want %q", outcome, checkpoint.OutcomeDeniedByPolicy)
		}
		// And nothing in the worker's tenant, which is where every one of them used to land.
		inOps, err := s.CheckpointerFor(workerTenant).Load(ctx, runID)
		if err != nil {
			t.Fatalf("Load from %s: %v", workerTenant, err)
		}
		if n := len(inOps.Records()); n != 0 {
			t.Fatalf("%s holds %d journal records for a run belonging to %s", workerTenant, n, tenantB)
		}
	})
}

// twoTenantActivityPolicySet writes one bundle per tenant into a temp directory and loads it through
// the real LoadSetFromDir, so the file-name-is-the-tenant rule is exercised rather than bypassed.
func twoTenantActivityPolicySet(t *testing.T, agentRef, activityName string, queueByTenant map[string]string) *policy.Set {
	t.Helper()
	dir := t.TempDir()
	for tenant, queue := range queueByTenant {
		// `policies:`, and the key matters: my first version wrote `statements:`, which YAML ignores
		// as an unknown field, so the bundle loaded with ZERO policies and denied everything. Fail-closed,
		// so not a hole — but the only symptom was "denied by policy" with nothing saying the file had
		// been misread. LoadEngine now refuses an empty bundle for exactly that reason.
		bundle := fmt.Sprintf(`apiVersion: harness.ai/v1
kind: PolicyBundle
cedarVersion: "4.0"
policies:
  - id: allow-%s
    effect: permit
    cedarSource: |
      permit(
        principal == Agent::"%s",
        action,
        resource
      ) when {
        resource is ExternalActivity &&
        resource.name == "%s" &&
        resource.task_queue == "%s"
      };
`, queue, agentRef, activityName, queue)
		if err := os.WriteFile(filepath.Join(dir, tenant+".yaml"), []byte(bundle), 0o600); err != nil {
			t.Fatalf("writing %s's bundle: %v", tenant, err)
		}
	}
	set, err := policy.LoadSetFromDir(dir)
	if err != nil {
		t.Fatalf("LoadSetFromDir: %v", err)
	}
	return set
}

// workerAuthWrap builds the caller Veritium's criteria describe: a service caller in its own tenant,
// entitled to act for a listed set of others.
func workerAuthWrap(t *testing.T, mux *http.ServeMux, tenant string, actsForTenants []string, actsAs ...string) http.Handler {
	t.Helper()
	a, err := auth.Load(auth.CallerBundleDoc{Kind: "CallerBundle", Callers: []auth.Caller{{
		ID:               "test-worker-" + tenant,
		Kind:             auth.KindService,
		Tenant:           tenant,
		TokenSHA256:      auth.HashToken(testCallerToken),
		MayActAs:         actsAs,
		MayActForTenants: actsForTenants,
		MayApprove:       true,
	}}})
	if err != nil {
		t.Fatalf("building the worker authenticator: %v", err)
	}
	return auth.Require(a)(httpserver.ExtractTraceContext(mux))
}

// postActivityPolicy posts /check-activity-policy with the run-tenant header, or without it when
// runTenant is empty — which is the negative control's shape.
func postActivityPolicy(t *testing.T, srv *httptest.Server, runTenant, agentRef, activityName, taskQueue string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(activityPolicyRequest{
		AgentManifestRef: agentRef, ActivityName: activityName, TaskQueue: taskQueue,
	})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/check-activity-policy", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if runTenant != "" {
		req.Header.Set(RunTenantHeader, runTenant)
	}
	resp, err := http.DefaultClient.Do(authorize(req))
	if err != nil {
		t.Fatalf("POST /check-activity-policy: %v", err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

func checkActivityPolicy(t *testing.T, srv *httptest.Server, runTenant, agentRef, activityName, taskQueue string) (bool, map[string]any) {
	t.Helper()
	status, body := postActivityPolicy(t, srv, runTenant, agentRef, activityName, taskQueue)
	if status != http.StatusOK {
		return false, body
	}
	// BOTH SPELLINGS, which is what the real client does. The response mixes cases — `Allowed`,
	// `CedarDecision` and `PolicyID` come out as Go field names while `disposition` and
	// `disposition_declared` are tagged — and activity_policy_activities.py reads
	// `body.get("Allowed", body.get("allowed", False))` with a comment saying so. Reading only the
	// lowercase one made this test report a denial on an allow (measured: PolicyID allow-b-online
	// with Allowed true), which would have had me debugging Cedar instead of the key.
	allowed, ok := body["Allowed"].(bool)
	if !ok {
		allowed, _ = body["allowed"].(bool)
	}
	return allowed, body
}

// postDecideAs is postDecide with the run-tenant header.
func postDecideAs(t *testing.T, srv *httptest.Server, runTenant string, body decideRequest) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/decide", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if runTenant != "" {
		req.Header.Set(RunTenantHeader, runTenant)
	}
	resp, err := http.DefaultClient.Do(authorize(req))
	if err != nil {
		t.Fatalf("POST /decide: %v", err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

var _ = store.CostEntry{}
