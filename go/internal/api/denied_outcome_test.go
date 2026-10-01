package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aeon-ai/aeon/go/internal/checkpoint"
	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/runcontroller"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// deniedOutcomeAgent is the agent in the checked-in policy bundle. A PERMITTED agent on purpose: it is
// allowed several tools, so a denial against it proves the refusal is per-tool and not just an unknown
// principal falling through. The bundle is the real examples/deep-research/policy_bundle.yaml.
const deniedOutcomeAgent = "deep-research-general@0.1.0"

// loadRunState fetches a run's journal through the seam's own HTTP surface.
//
// Through the HTTP surface and not the Go interface on purpose: the party that has to read these
// records is a Python loop, so a record only counts as journalled if it comes back out the way that
// loop will fetch it. A test reading the Checkpointer directly would pass on a serialisation bug that
// made the outcome unreadable to the only consumer that matters.
func loadRunState(t *testing.T, srv *httptest.Server, runID string) *checkpoint.RunState {
	t.Helper()
	resp := getAuthed(t, srv.URL+"/runs/"+runID+"/checkpoints")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET journal for %s = %d, want 200", runID, resp.StatusCode)
	}
	var parsed struct {
		Records []checkpoint.Record `json:"records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decoding journal: %v", err)
	}
	return checkpoint.NewRunState(runID, parsed.Records)
}

// newDeniedOutcomeGateway is a real Tool Gateway with a real Cedar engine over the checked-in policy
// bundle, and a real Postgres-backed journal. The journal is mounted alongside it so the test reads
// the records back the same way the framework does.
func newDeniedOutcomeGateway(t *testing.T) *httptest.Server {
	t.Helper()
	raw, err := os.ReadFile(repoPolicyBundlePath(t))
	if err != nil {
		t.Fatalf("reading policy bundle: %v", err)
	}
	var doc policy.PolicyBundleDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing policy bundle: %v", err)
	}
	engine, err := policy.LoadEngine(doc)
	if err != nil {
		t.Fatalf("loading Cedar engine: %v", err)
	}

	s, err := store.Connect(context.Background(), memoryTestDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	// An executor that FAILS the test if it is ever reached. The denial has to happen before the effect,
	// and a test that only inspected the journal would still pass if the tool had also run — which is the
	// one outcome that would make the record a lie.
	executor := toolexec.NewExecutor()
	executor.Register("shell.exec", func(map[string]any) (map[string]any, error) {
		t.Error("the executor ran a tool the policy denied — the journal record would be describing a call that actually happened")
		return map[string]any{}, nil
	})

	mux := http.NewServeMux()
	(&ToolGatewayHandlers{Policy: engine, Executor: executor, Checkpointer: s.Checkpointer()}).Register(mux)
	(&CheckpointHandlers{Checkpointer: s.Checkpointer()}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux, deniedOutcomeAgent))
	t.Cleanup(srv.Close)
	return srv
}

func postExecuteForRun(t *testing.T, srv *httptest.Server, body toolCallRequest) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp := postJSONAuthed(t, srv.URL+"/execute", raw)
	defer resp.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decoding /execute response: %v", err)
	}
	return resp.StatusCode, parsed
}

// TestDeniedStepIsJournalledAsKnownOutcome is INT-011's acceptance test.
//
// THE BUG IT PINS, in one sentence: a step denied BEFORE executing had no `completed` record, and
// neither did a step that was attempted and whose fate nobody knows — so the two were the same thing to
// anyone resuming the run, and a run suspended waiting for a person could not be resumed at all.
// Synaptum found it by persisting their journal for real; it does not appear until durability does,
// which is why every in-memory test stayed green through it.
//
// Two paths, one bug, and both are here because they fail independently:
//
//	(a) a person's approval decision, written by the HARNESS — the person decides while the loop is not
//	    running, so the loop cannot record what it never witnessed;
//	(b) a Cedar denial at the Tool Gateway, which is default-deny and therefore also covers the unknown
//	    tool and the unlisted principal.
//
// Real infrastructure on both: a real Cedar engine over the checked-in bundle, a real Postgres journal,
// and for (a) a real Temporal worker deciding a real suspended run.
func TestDeniedStepIsJournalledAsKnownOutcome(t *testing.T) {
	t.Run("a Cedar denial is a completed step with a denied outcome", func(t *testing.T) {
		srv := newDeniedOutcomeGateway(t)
		runID := newRunID("denied-policy")

		status, body := postExecuteForRun(t, srv, toolCallRequest{
			AgentManifestRef: deniedOutcomeAgent,
			ToolName:         "shell.exec",
			Args:             map[string]any{"cmd": "rm -rf /"},
			RunID:            runID,
			StepID:           "n0",
		})
		if status != http.StatusForbidden {
			t.Fatalf("POST /execute = %d, want 403", status)
		}
		if allowed, _ := body["allowed"].(bool); allowed {
			t.Fatal("the gateway reported the call as allowed")
		}
		if journalled, _ := body["journalled"].(bool); !journalled {
			t.Fatalf("the denial was not journalled: %v", body["journal"])
		}

		// The point of the whole feature: the step now HAS an outcome, where before it had nothing.
		state := loadRunState(t, srv, runID)
		outcome, reason, ok := state.StepOutcome("n0")
		if !ok {
			t.Fatal("the denied step has no completed record — it is still indistinguishable from a step whose fate nobody knows, which is the exact bug INT-011 is about")
		}
		if outcome != checkpoint.OutcomeDeniedByPolicy {
			t.Errorf("outcome = %q, want %q", outcome, checkpoint.OutcomeDeniedByPolicy)
		}
		if !outcome.Denied() {
			t.Errorf("Denied() is false for %q — a loop asking the one question that decides whether to continue gets the wrong answer", outcome)
		}
		// The reason NAMES the policy. "denied by policy" alone sends whoever is debugging back to reading
		// the whole bundle, which for a person woken at 3am is the same as recording nothing.
		if !strings.Contains(reason, "forbid-shell-for-everyone") {
			t.Errorf("reason = %q: it does not name the policy that refused, so the record says a step was denied and nothing about by what", reason)
		}

		// A denied step is NOT "attempted". Both used to be an absence of a completed record, and the
		// distinction between them is the entire point.
		if state.Attempted("n0") {
			t.Error("the denied step reports Attempted — the two states INT-011 separated have collapsed back together")
		}
	})

	t.Run("default-deny is journalled too, and says that is what happened", func(t *testing.T) {
		// Cedar is default-deny, so the commonest real denial is not a `forbid` at all — it is a tool no
		// policy permits, which produces NO policy id. That distinction is worth carrying into the record:
		// "nothing permitted this" and "something forbade this" send a reader to different places, and only
		// the first is usually a missing entry in the bundle rather than a genuine violation.
		srv := newDeniedOutcomeGateway(t)
		runID := newRunID("denied-default")

		// A tool NO policy in the bundle names. It used to be artifact.write, and INT-010 gave that one an
		// explicit permit with disposition require_approval — at which point it stopped being default-denied
		// and stopped being journalled at all, because a step waiting for a person has no outcome yet. The
		// test caught it, which is the behaviour wanted; the fixture just has to name something the bundle
		// genuinely does not mention.
		status, _ := postExecuteForRun(t, srv, toolCallRequest{
			AgentManifestRef: deniedOutcomeAgent, ToolName: "nothing.in.the.bundle.names.this",
			Args: map[string]any{}, RunID: runID, StepID: "n0",
		})
		if status != http.StatusForbidden {
			t.Fatalf("POST /execute = %d, want 403", status)
		}

		outcome, reason, ok := loadRunState(t, srv, runID).StepOutcome("n0")
		if !ok {
			t.Fatal("a default-denied step has no record")
		}
		if outcome != checkpoint.OutcomeDeniedByPolicy {
			t.Errorf("outcome = %q, want %q", outcome, checkpoint.OutcomeDeniedByPolicy)
		}
		if !strings.Contains(reason, "default-deny") {
			t.Errorf("reason = %q: it does not say that nothing permitted the call, which is a different problem from something forbidding it", reason)
		}
	})

	t.Run("a denied step carries no result a loop could mistake for one", func(t *testing.T) {
		// The failure mode of the fix itself. Recording the denial in the SAME place a result goes would
		// hand a loop that does not inspect the outcome something that parses as a successful payload. So
		// `result` must be absent on a denied record, and that is asserted rather than assumed.
		srv := newDeniedOutcomeGateway(t)
		runID := newRunID("denied-no-result")

		postExecuteForRun(t, srv, toolCallRequest{
			AgentManifestRef: deniedOutcomeAgent, ToolName: "shell.exec",
			Args: map[string]any{}, RunID: runID, StepID: "n0",
		})

		rec, ok := loadRunState(t, srv, runID).Completed("n0")
		if !ok {
			t.Fatal("no completed record for the denied step")
		}
		var payload map[string]any
		if err := json.Unmarshal(rec.Payload, &payload); err != nil {
			t.Fatalf("the payload is not an object: %v", err)
		}
		if _, hasResult := payload["result"]; hasResult {
			t.Errorf("the denied record carries a `result` key: a loop reading the payload for a result would find one for a call that never ran (%s)", rec.Payload)
		}
		if payload["outcome"] != string(checkpoint.OutcomeDeniedByPolicy) {
			t.Errorf("payload outcome = %v, want %q — the outcome has to be unmissable at the top level, not nested where a reader can skip it", payload["outcome"], checkpoint.OutcomeDeniedByPolicy)
		}
	})

	t.Run("a repeated denial is the same fact, not a second one", func(t *testing.T) {
		// At-least-once execution means the gateway WILL see the same denied call twice. The journal's
		// idempotency is by (run_id, step_id, phase), so the retry must collapse — and the response has to
		// say so rather than reporting a fresh record that does not exist.
		srv := newDeniedOutcomeGateway(t)
		runID := newRunID("denied-retry")
		req := toolCallRequest{
			AgentManifestRef: deniedOutcomeAgent, ToolName: "shell.exec",
			Args: map[string]any{}, RunID: runID, StepID: "n0",
		}

		postExecuteForRun(t, srv, req)
		_, second := postExecuteForRun(t, srv, req)

		journal, _ := second["journal"].(map[string]any)
		if dup, _ := journal["duplicate"].(bool); !dup {
			t.Errorf("the second denial did not report duplicate=true: %v", journal)
		}
		if records := loadRunState(t, srv, runID).Records(); len(records) != 1 {
			t.Errorf("the journal has %d records for one denied step, want 1", len(records))
		}
	})

	t.Run("no run_id means unjournalled and SAID so, not silently unrecorded", func(t *testing.T) {
		// Mode C: an MCP client with no Aeon run behind it. There is nothing to journal against, and that
		// must neither fail the denial nor look like a recorded one. Three states, not two — the same rule
		// this codebase applies to every counter, because "nothing to record against" and "the write
		// failed" are different facts and only the second is a problem.
		srv := newDeniedOutcomeGateway(t)

		status, body := postExecuteForRun(t, srv, toolCallRequest{
			AgentManifestRef: deniedOutcomeAgent, ToolName: "shell.exec", Args: map[string]any{},
		})
		if status != http.StatusForbidden {
			t.Fatalf("POST /execute = %d, want 403 — a call with no run is still denied", status)
		}
		if journalled, _ := body["journalled"].(bool); journalled {
			t.Fatal("reported as journalled with no run_id to journal against")
		}
		journal, _ := body["journal"].(map[string]any)
		if reason, _ := journal["reason"].(string); reason == "" {
			t.Error("journalled=false with no reason: the reader cannot tell 'nothing to record against' from 'the journal is down'")
		}
	})

	t.Run("a person's rejection is journalled by the harness", func(t *testing.T) {
		// Path (a), and the one that made this feature necessary: a real run suspended on a real approval,
		// rejected by a real HTTP call, with the record written by the harness because the loop is parked in
		// wait_condition and witnesses nothing.
		c := testTemporalClient(t)
		s, err := store.Connect(context.Background(), memoryTestDSN(t))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(s.Close)

		mux := http.NewServeMux()
		(&RunControllerHandlers{
			Controller: runcontroller.New(c, ""), Checkpointer: s.Checkpointer(),
		}).Register(mux)
		(&CheckpointHandlers{Checkpointer: s.Checkpointer()}).Register(mux)
		srv := httptest.NewServer(authWrap(t, mux, deniedOutcomeAgent))
		t.Cleanup(srv.Close)

		runID := newRunID("denied-approval")
		startRun(t, srv, runID, approvalGraph("denied-approval-test.txt"))

		paused := waitForStatus(t, srv, runID, "PAUSED_FOR_APPROVAL", 15*time.Second)
		pending, ok := paused["pending_approval"].(map[string]any)
		if !ok {
			t.Fatalf("expected pending_approval in status, got %v", paused)
		}
		approvalID, _ := pending["approval_id"].(string)
		toolCallHash, _ := pending["tool_call_hash"].(string)
		if approvalID == "" || toolCallHash == "" {
			t.Fatalf("pending approval is missing its identity: %v", pending)
		}

		resp := postApprovalDecision(t, srv, runID, "reject", toolCallHash)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST /runs/%s/reject = %d, want 202", runID, resp.StatusCode)
		}
		var decision map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
			t.Fatalf("decoding the decision response: %v", err)
		}
		if journalled, _ := decision["journalled"].(bool); !journalled {
			t.Fatalf("the rejection was not journalled: %v", decision["journal"])
		}

		// Keyed by the APPROVAL, which is the convention the three teams share — ApprovalStep rather than a
		// hand-written prefix, so nobody derives it differently.
		stepID := checkpoint.ApprovalStep(approvalID)
		state := loadRunState(t, srv, runID)
		outcome, _, ok := state.StepOutcome(stepID)
		if !ok {
			t.Fatalf("no record for %s — the person's decision left no trace, so a resuming loop cannot tell a rejected run from one still waiting", stepID)
		}
		if outcome != checkpoint.OutcomeApprovalDenied {
			t.Errorf("outcome = %q, want %q", outcome, checkpoint.OutcomeApprovalDenied)
		}
		if !outcome.Denied() {
			t.Errorf("Denied() is false for %q", outcome)
		}

		// The hash travels with the record. Without it the journal says a person said no, but not to WHAT —
		// and an approval that cannot be tied back to the parameters it covered is the hole the triple
		// (step_id, tool_name, tool_args) was agreed to close.
		rec, _ := state.Completed(stepID)
		if !bytes.Contains(rec.Payload, []byte(toolCallHash)) {
			t.Errorf("the record does not carry the tool_call_hash the person decided on: %s", rec.Payload)
		}

		// And the run itself must still fail. A journalled denial that let the call through would be a
		// record of a refusal that did not refuse.
		waitForStatus(t, srv, runID, "FAILED", 15*time.Second)
	})

	t.Run("a granted approval is journalled as granted, not merely as decided", func(t *testing.T) {
		// The counterpart, and it is not symmetry for its own sake: if both decisions recorded the same
		// outcome the field would be worthless, and a test that only checked the rejection would not notice.
		c := testTemporalClient(t)
		s, err := store.Connect(context.Background(), memoryTestDSN(t))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(s.Close)

		mux := http.NewServeMux()
		(&RunControllerHandlers{
			Controller: runcontroller.New(c, ""), Checkpointer: s.Checkpointer(),
		}).Register(mux)
		(&CheckpointHandlers{Checkpointer: s.Checkpointer()}).Register(mux)
		srv := httptest.NewServer(authWrap(t, mux, deniedOutcomeAgent))
		t.Cleanup(srv.Close)

		runID := newRunID("granted-approval")
		startRun(t, srv, runID, approvalGraph("granted-approval-test.txt"))
		paused := waitForStatus(t, srv, runID, "PAUSED_FOR_APPROVAL", 15*time.Second)
		pending, _ := paused["pending_approval"].(map[string]any)
		approvalID, _ := pending["approval_id"].(string)
		toolCallHash, _ := pending["tool_call_hash"].(string)

		resp := postApprovalDecision(t, srv, runID, "approve", toolCallHash)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST /runs/%s/approve = %d, want 202", runID, resp.StatusCode)
		}

		outcome, _, ok := loadRunState(t, srv, runID).StepOutcome(checkpoint.ApprovalStep(approvalID))
		if !ok {
			t.Fatal("the granted approval was not journalled")
		}
		if outcome != checkpoint.OutcomeApprovalGranted {
			t.Errorf("outcome = %q, want %q", outcome, checkpoint.OutcomeApprovalGranted)
		}
		if outcome.Denied() {
			t.Errorf("Denied() is true for %q — a granted approval would stop a run that a person allowed", outcome)
		}

		// SEC-005: WHO approved, on a real run that really suspended and was really let through.
		//
		// Before this the journal said `approval_granted` with the reason "decided by a person via the
		// run controller" — and nothing verified any of that: the endpoint was unauthenticated, so the
		// only true statement was "something that could reach the port". An audit line asserting a
		// person was involved, on the one step whose entire purpose is that a person was involved.
		decidedBy, named := loadRunState(t, srv, runID).ApprovalActor(checkpoint.ApprovalStep(approvalID))
		if !named {
			t.Fatalf("the approval record names no decider, so the audit trail of an irreversible call " +
				"says it was approved and cannot say by whom")
		}
		if decidedBy != "test-caller" {
			t.Fatalf("decided_by = %q, want the authenticated caller's id", decidedBy)
		}

		waitForStatus(t, srv, runID, "SUCCEEDED", 15*time.Second)
	})
}

// TestPreInt011RecordsStillMeanWhatTheyMeant guards the migration this feature did NOT get to do.
//
// Every `completed` record written before INT-011 carries a bare result and no outcome field, and there
// are such records in real journals right now. Reading one as anything other than a result would strand
// a step that really did run — so the absence of an outcome means OutcomeResult, and the payload that
// cannot be parsed as an envelope means the same. Asserted here because the alternative is discovering
// it on a resumed run.
func TestPreInt011RecordsStillMeanWhatTheyMeant(t *testing.T) {
	rec := func(payload string) checkpoint.Record {
		return checkpoint.Record{Entry: checkpoint.Entry{
			RunID: "r", StepID: "s", Phase: checkpoint.PhaseCompleted, Payload: json.RawMessage(payload),
		}, Seq: 1}
	}

	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"an old result object", `{"bytes_written":12}`},
		{"an old result that happens to have a reason field", `{"reason":"because"}`},
		{"a payload that is not an object at all", `"just a string"`},
		{"no payload", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := checkpoint.NewRunState("r", []checkpoint.Record{rec(tc.payload)})
			outcome, _, ok := state.StepOutcome("s")
			if !ok {
				t.Fatal("a pre-INT-011 completed record stopped counting as completed")
			}
			if outcome != checkpoint.OutcomeResult {
				t.Errorf("outcome = %q, want %q", outcome, checkpoint.OutcomeResult)
			}
			if outcome.Denied() {
				t.Error("a pre-INT-011 result reads as denied — every step of every older run would look refused")
			}
		})
	}
}
