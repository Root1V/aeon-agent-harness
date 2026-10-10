package api

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	aeonmcp "github.com/aeon-ai/aeon/go/internal/mcp"
	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// TestEveryDoorRecordsTheInvocation is INT-013's acceptance test, against real Postgres.
//
// THE ASSERTION IS PER DOOR AND NOT "recording works", because the defect was never that recording
// was broken — it was that WHICH DOOR YOU USED decided whether your call existed in the record. A
// call carrying an idempotency key left a durable row in tool_executions; the same call without one
// left nothing, and every MCP call left nothing at all, including its denials. A test that drove one
// door and asserted one row would have passed before this feature existed on the keyed path and
// still missed the two silent ones.
func TestEveryDoorRecordsTheInvocation(t *testing.T) {
	dsn := memoryTestDSN(t)
	ctx := context.Background()
	s, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	const tenant = "default"
	executor := toolexec.NewExecutor()
	executor.Register("probe.ok", func(_ string, args map[string]any) (map[string]any, error) {
		return map[string]any{"echo": args["value"]}, nil
	})
	executor.Register("probe.boom", func(_ string, _ map[string]any) (map[string]any, error) {
		return nil, fmt.Errorf("the tool itself failed, loudly")
	})
	// THE SAME adapter cmd/aeon-toolgw wires, not a copy of it. It used to be a copy, and the copy
	// is how INT-014's two new fields got dropped in both places at once.
	executor.WithRecorder(ToolInvocationRecorder(s, tenant))

	engine := permitAllEngine(t)
	mux := http.NewServeMux()
	(&ToolGatewayHandlers{
		Policy: policy.SingleTenantSet(tenant, engine), Executor: executor, Executions: s,
	}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux, "deep-research-general@0.1.0"))
	t.Cleanup(srv.Close)

	log := s.ToolInvocationsFor(tenant)

	t.Run("the plain HTTP door records, with the run attribution the caller supplied", func(t *testing.T) {
		runID := "run-" + invSuffix(t)
		status, body := postExecuteBody(t, srv, map[string]any{
			"agent_manifest_ref": "deep-research-general@0.1.0",
			"tool_name":          "probe.ok",
			"args":               map[string]any{"value": "a"},
			"run_id":             runID,
			"step_id":            "step-1",
		})
		if status != http.StatusOK {
			t.Fatalf("status = %d, body = %v", status, body)
		}
		// The response says so too, exactly as INT-011 reports `journalled` on a denial: a caller
		// must be able to tell "executed and recorded" from "executed, and nobody wrote it down".
		if body["recorded"] != true {
			t.Errorf(`response recorded = %v, want true — the body must report that the fact was written`, body["recorded"])
		}

		rows, err := log.ForRun(ctx, runID, 10)
		if err != nil {
			t.Fatalf("ForRun: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows for %s = %d, want 1", runID, len(rows))
		}
		got := rows[0]
		if got.Door != toolexec.DoorHTTP || got.Outcome != store.ToolInvocationOK {
			t.Errorf("door/outcome = %q/%q, want %q/%q", got.Door, got.Outcome, toolexec.DoorHTTP, store.ToolInvocationOK)
		}
		if got.StepID != "step-1" || got.AgentManifestRef != "deep-research-general@0.1.0" {
			t.Errorf("attribution lost: step=%q agent=%q", got.StepID, got.AgentManifestRef)
		}
		if got.ErrorMessage != "" {
			t.Errorf("a successful call recorded an error message: %q", got.ErrorMessage)
		}
	})

	t.Run("the idempotency door records too, and under the same shape", func(t *testing.T) {
		runID := "run-" + invSuffix(t)
		status, body := postExecuteBody(t, srv, map[string]any{
			"agent_manifest_ref": "deep-research-general@0.1.0",
			"tool_name":          "probe.ok",
			"args":               map[string]any{"value": "b"},
			"idempotency_key":    "key-" + invSuffix(t),
			"run_id":             runID,
			"step_id":            "step-1",
		})
		if status != http.StatusOK {
			t.Fatalf("status = %d, body = %v", status, body)
		}
		rows, err := log.ForRun(ctx, runID, 10)
		if err != nil {
			t.Fatalf("ForRun: %v", err)
		}
		if len(rows) != 1 || rows[0].Door != toolexec.DoorHTTPDedupe {
			t.Fatalf("rows = %+v, want exactly one through %q", rows, toolexec.DoorHTTPDedupe)
		}
	})

	t.Run("the MCP door records, and says honestly that there was no run", func(t *testing.T) {
		toolName := "probe.ok"
		row := &store.ToolRecord{
			ToolID: toolName + ".1.0.0", Version: "1.0.0", Name: toolName,
			Descriptor: map[string]any{"description": "probe", "input_schema": map[string]any{"type": "object"}},
		}
		mcpServer, _ := aeonmcp.NewToolGatewayCatalog([]*store.ToolRecord{row}, engine, executor)

		clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
		if _, err := mcpServer.Connect(ctx, serverTransport, nil); err != nil {
			t.Fatalf("mcp server connect: %v", err)
		}
		cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "int-013-test", Version: "0"}, nil).
			Connect(ctx, clientTransport, nil)
		if err != nil {
			t.Fatalf("mcp client connect: %v", err)
		}
		defer cs.Close()

		before := countForTool(t, ctx, log, toolName, toolexec.DoorMCP)
		res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: toolName, Arguments: map[string]any{"value": "c"}})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("the probe tool errored: %+v", res.Content)
		}
		// `_meta` and not the text, so a consumer reads the same fact the HTTP doors put in a field.
		if res.Meta["aeon.tool.invocation_recorded"] != true {
			t.Errorf("_meta recorded = %v, want true (meta = %v)", res.Meta["aeon.tool.invocation_recorded"], res.Meta)
		}

		after := countForTool(t, ctx, log, toolName, toolexec.DoorMCP)
		if after != before+1 {
			t.Fatalf("mcp rows went %d -> %d, want exactly one more — before INT-013 this door recorded nothing", before, after)
		}
		rows, err := log.ForRun(ctx, "", 50)
		if err != nil {
			t.Fatalf("ForRun(\"\"): %v", err)
		}
		if len(rows) == 0 {
			t.Fatal("no row with an empty run_id: an external MCP caller is not a run, and the row must say so rather than invent an attribution")
		}
	})

	t.Run("a tool that fails is recorded as a failure, with its message", func(t *testing.T) {
		runID := "run-" + invSuffix(t)
		status, _ := postExecuteBody(t, srv, map[string]any{
			"agent_manifest_ref": "deep-research-general@0.1.0",
			"tool_name":          "probe.boom",
			"args":               map[string]any{},
			"run_id":             runID,
			"step_id":            "step-1",
		})
		if status == http.StatusOK {
			t.Fatalf("status = %d, want a failure", status)
		}
		rows, err := log.ForRun(ctx, runID, 10)
		if err != nil {
			t.Fatalf("ForRun: %v", err)
		}
		// A log that only holds successes answers the wrong question during an incident.
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1: a tool that ran and failed is as much a fact as one that worked", len(rows))
		}
		if rows[0].Outcome != store.ToolInvocationError || rows[0].ErrorMessage == "" {
			t.Errorf("outcome/message = %q/%q, want %q with a message", rows[0].Outcome, rows[0].ErrorMessage, store.ToolInvocationError)
		}
	})

	// INT-014's three resolutions that never pass through Execute. They are in THIS test and not a
	// separate one on purpose: the claim is about the set of ways a tool call can be resolved, and
	// splitting it is how one of them ends up uncovered.
	t.Run("a refusal is recorded even when there is no run to journal it against", func(t *testing.T) {
		denyMux := http.NewServeMux()
		(&ToolGatewayHandlers{
			Policy: policy.SingleTenantSet(tenant, denyEverythingEngine(t)), Executor: executor, Executions: s,
		}).Register(denyMux)
		denySrv := httptest.NewServer(authWrap(t, denyMux, "deep-research-general@0.1.0"))
		t.Cleanup(denySrv.Close)

		// NO run_id and NO step_id, which is the case journalDenial gives up on in its own words
		// ("there is no journal to record this against") — and the whole reason this row has to exist
		// somewhere else.
		status, body := postExecuteBody(t, denySrv, map[string]any{
			"agent_manifest_ref": "deep-research-general@0.1.0",
			"tool_name":          "probe.ok",
			"args":               map[string]any{},
		})
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", status, body)
		}
		if body["journalled"] == true {
			t.Fatalf("the run journal claims it recorded a call with no run_id: %v", body)
		}
		if body["recorded"] != true {
			t.Errorf("recorded = %v, want true: the refusal must be in the bitácora even with no run", body["recorded"])
		}

		rows, err := log.ForTool(ctx, "probe.ok", 500)
		if err != nil {
			t.Fatalf("ForTool: %v", err)
		}
		var denials int
		for _, r := range rows {
			if r.Outcome == store.ToolInvocationDenied && r.Door == toolexec.DoorHTTP {
				denials++
				if r.Disposition == "" {
					t.Error(`a denial with an empty disposition: "refused" and "refused, but a person could approve" are different facts (INT-010)`)
				}
				if r.DurationMS != nil {
					t.Errorf("a denial carries a duration (%d): nothing executed, so 0 would claim it ran instantly", *r.DurationMS)
				}
			}
		}
		if denials == 0 {
			t.Fatal("no denial row through the HTTP door")
		}
	})

	t.Run("the MCP door records its refusals too, which it never did before", func(t *testing.T) {
		toolName := "probe.ok"
		row := &store.ToolRecord{
			ToolID: toolName + ".1.0.0", Version: "1.0.0", Name: toolName,
			Descriptor: map[string]any{"description": "probe", "input_schema": map[string]any{"type": "object"}},
		}
		mcpServer, _ := aeonmcp.NewToolGatewayCatalog([]*store.ToolRecord{row}, denyEverythingEngine(t), executor)
		clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
		if _, err := mcpServer.Connect(ctx, serverTransport, nil); err != nil {
			t.Fatalf("mcp server connect: %v", err)
		}
		cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "int-014-test", Version: "0"}, nil).
			Connect(ctx, clientTransport, nil)
		if err != nil {
			t.Fatalf("mcp client connect: %v", err)
		}
		defer cs.Close()

		before := countDenials(t, ctx, log, toolName, toolexec.DoorMCP)
		res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: toolName, Arguments: map[string]any{}})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !res.IsError {
			t.Fatal("the call was not refused, so this subtest is measuring nothing")
		}
		if res.Meta["aeon.tool.invocation_recorded"] != true {
			t.Errorf("_meta recorded = %v, want true (meta = %v)", res.Meta["aeon.tool.invocation_recorded"], res.Meta)
		}
		if after := countDenials(t, ctx, log, toolName, toolexec.DoorMCP); after != before+1 {
			t.Fatalf("mcp denial rows went %d -> %d, want exactly one more", before, after)
		}
	})

	t.Run("a replay is recorded, which the dedupe table structurally cannot show", func(t *testing.T) {
		runID := "run-" + invSuffix(t)
		call := map[string]any{
			"agent_manifest_ref": "deep-research-general@0.1.0",
			"tool_name":          "probe.ok",
			"args":               map[string]any{"value": "replay"},
			"idempotency_key":    "key-" + invSuffix(t),
			"run_id":             runID,
			"step_id":            "step-1",
		}
		if status, body := postExecuteBody(t, srv, call); status != http.StatusOK {
			t.Fatalf("first call: status = %d, body = %v", status, body)
		}
		status, body := postExecuteBody(t, srv, call)
		if status != http.StatusOK {
			t.Fatalf("replay: status = %d, body = %v", status, body)
		}
		if body["deduplicated"] != true {
			t.Fatalf("the second call executed instead of replaying, so this measures nothing: %v", body)
		}
		if body["recorded"] != true {
			t.Errorf("recorded = %v, want true on a replay", body["recorded"])
		}

		rows, err := log.ForRun(ctx, runID, 10)
		if err != nil {
			t.Fatalf("ForRun: %v", err)
		}
		// TWO rows for ONE idempotency key, which is the whole point: the dedupe table holds one row
		// per key and a replay leaves it untouched, so a bitácora that also collapsed them would
		// undercount exactly the retries TOOL-005 exists to absorb.
		var executed, replayed int
		for _, r := range rows {
			switch r.Outcome {
			case store.ToolInvocationOK:
				executed++
			case store.ToolInvocationReplayed:
				replayed++
				if r.DurationMS != nil {
					t.Errorf("a replay carries a duration (%d): nothing executed", *r.DurationMS)
				}
			}
		}
		if executed != 1 || replayed != 1 {
			t.Fatalf("executed=%d replayed=%d (rows=%+v), want exactly one of each", executed, replayed, rows)
		}
	})

	// NEGATIVE CONTROL. An unknown tool must leave NO row: nothing ran, so a row would assert an
	// execution that never happened. This is the control on WHERE the recording sits — move it before
	// the dispatch (which looks like a simplification, since the invocation is already in hand) and
	// this subtest fails while every other one still passes.
	t.Run("an unknown tool is not recorded, because nothing ran", func(t *testing.T) {
		runID := "run-" + invSuffix(t)
		status, _ := postExecuteBody(t, srv, map[string]any{
			"agent_manifest_ref": "deep-research-general@0.1.0",
			"tool_name":          "probe.does-not-exist",
			"args":               map[string]any{},
			"run_id":             runID,
			"step_id":            "step-1",
		})
		if status == http.StatusOK {
			t.Fatalf("status = %d, want a failure for an unknown tool", status)
		}
		rows, err := log.ForRun(ctx, runID, 10)
		if err != nil {
			t.Fatalf("ForRun: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("rows = %+v, want none: an unknown tool never executed, so there is no invocation to record", rows)
		}
	})
}

// countForTool counts this tenant's rows for one tool arriving through one door.
func countForTool(t *testing.T, ctx context.Context, log *store.ToolInvocations, toolName, door string) int {
	t.Helper()
	rows, err := log.ForTool(ctx, toolName, 500)
	if err != nil {
		t.Fatalf("ForTool: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Door == door {
			n++
		}
	}
	return n
}

func invSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return fmt.Sprintf("%x", b)
}

func permitAllEngine(t *testing.T) *policy.Engine {
	t.Helper()
	eng, err := policy.LoadEngine(policy.PolicyBundleDoc{Policies: []policy.PolicyBundleItem{
		{ID: "int-013-permit-all", Effect: "permit", CedarSource: `permit(principal, action, resource);`},
	}})
	if err != nil {
		t.Fatalf("LoadEngine: %v", err)
	}
	return eng
}

func countDenials(t *testing.T, ctx context.Context, log *store.ToolInvocations, toolName, door string) int {
	t.Helper()
	rows, err := log.ForTool(ctx, toolName, 500)
	if err != nil {
		t.Fatalf("ForTool: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Door == door && r.Outcome == store.ToolInvocationDenied {
			n++
		}
	}
	return n
}

// denyEverythingEngine refuses every tool, so the refusal paths are reached for real rather than
// simulated by calling RecordRefusal directly from the test.
func denyEverythingEngine(t *testing.T) *policy.Engine {
	t.Helper()
	eng, err := policy.LoadEngine(policy.PolicyBundleDoc{Policies: []policy.PolicyBundleItem{
		{ID: "int-014-deny-all", Effect: "permit", CedarSource: `permit(principal, action, resource) when { false };`},
	}})
	if err != nil {
		t.Fatalf("LoadEngine: %v", err)
	}
	return eng
}
