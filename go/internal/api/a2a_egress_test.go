package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aeon-ai/aeon/go/internal/a2a"
	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/secrets"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// fakeRemoteAgent is a real HTTP server speaking real A2A JSON-RPC over a real socket.
//
// Not the SDK's own server helper, and the reason is the point of half these subtests: the states that
// must be handled — one this build has never heard of, an absent one — are states a compliant SDK server
// CANNOT produce. A double that could only emit legal states would leave the fail-safe path untested,
// which is the path that matters, because it is the one a future A2A version will exercise in production.
//
// It records every request it received, so the test can assert on what a denied delegation did NOT do.
type fakeRemoteAgent struct {
	mu       sync.Mutex
	requests []recordedRequest
	// state is what the next response reports. Settable per subtest.
	state string
	// omitStatus makes the response a Message rather than a Task — a remote that answered inline.
	omitStatus bool
	taskID     string
}

type recordedRequest struct {
	authorization   string
	delegatingAgent string
	remoteAgentID   string
	hopDepth        string
	body            []byte
}

func (f *fakeRemoteAgent) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{
			authorization:   r.Header.Get("Authorization"),
			delegatingAgent: r.Header.Get(a2a.HeaderDelegatingAgent),
			remoteAgentID:   r.Header.Get(a2a.HeaderRemoteAgentID),
			hopDepth:        r.Header.Get(a2a.HeaderHopDepth),
			body:            body,
		})
		state, omit, taskID := f.state, f.omitStatus, f.taskID
		f.mu.Unlock()

		var result map[string]any
		if omit {
			result = map[string]any{"kind": "message", "messageId": "m1", "role": "agent"}
		} else {
			result = map[string]any{
				"kind": "task", "id": taskID, "contextId": "ctx1",
				"status": map[string]any{"state": state},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}
}

func (f *fakeRemoteAgent) received() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeRemoteAgent) set(state string, omitStatus bool, taskID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.omitStatus, f.taskID = state, omitStatus, taskID
}

// egressFixture is a real Tool Gateway egress proxy over a real Cedar engine, a real Postgres registry
// and ledger, and a real Secret Broker, pointed at a real remote HTTP server.
type egressFixture struct {
	proxy       *httptest.Server
	remote      *fakeRemoteAgent
	remoteURL   string
	delegations *store.A2ADelegations
	handlers    *A2AEgressHandlers
}

const (
	egressAgent      = "deep-research-general@0.1.0"
	allowedRemote    = "research-partner"
	remoteCredential = "a2a.research-partner.token"
)

func newEgressFixture(t *testing.T, maxInFlight int) *egressFixture {
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

	remote := &fakeRemoteAgent{state: "working", taskID: fmt.Sprintf("task-%d", time.Now().UnixNano())}
	remoteSrv := httptest.NewServer(remote.handler())
	t.Cleanup(remoteSrv.Close)

	// A real Secret Broker holding the destination's credential. The governed process never sees this
	// value; the proxy resolves it per call and revokes the lease immediately.
	broker := secrets.NewBroker(map[string]string{remoteCredential: "s3cr3t-remote-token"})

	if _, err := s.RemoteAgents().Declare(context.Background(), store.RemoteAgentRecord{
		AgentID: allowedRemote, URL: remoteSrv.URL, Risk: store.RiskHigh,
		Description: "A2A-002 acceptance test destination", CredentialSecretName: remoteCredential,
	}); err != nil {
		t.Fatalf("declaring the remote agent: %v", err)
	}
	// A declared destination that policy does NOT permit. Declared on purpose: it separates "nobody
	// declared this" from "policy refused this", which are the two refusals that look alike from outside.
	if _, err := s.RemoteAgents().Declare(context.Background(), store.RemoteAgentRecord{
		AgentID: "unpermitted-partner", URL: remoteSrv.URL, Risk: store.RiskCritical,
	}); err != nil {
		t.Fatalf("declaring the unpermitted remote agent: %v", err)
	}

	handlers := &A2AEgressHandlers{
		Policy: engine, RemoteAgents: s.RemoteAgents(), Delegations: s.A2ADelegations(),
		Broker: broker, MaxInFlightPerRun: maxInFlight, InFlightStaleAfter: time.Hour,
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	proxy := httptest.NewServer(mux)
	t.Cleanup(proxy.Close)

	return &egressFixture{
		proxy: proxy, remote: remote, remoteURL: remoteSrv.URL,
		delegations: s.A2ADelegations(), handlers: handlers,
	}
}

// delegate sends a real A2A JSON-RPC request through the proxy.
func (f *egressFixture) delegate(t *testing.T, remoteID, runID, method string, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"` + method +
		`","params":{"message":{"role":"user","parts":[{"kind":"text","text":"delegated work"}],"messageId":"m0","kind":"message"}}}`)

	req, err := http.NewRequest(http.MethodPost, f.proxy.URL+"/a2a/egress/"+remoteID, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(a2a.HeaderAgentManifestRef, egressAgent)
	if runID != "" {
		req.Header.Set(a2a.HeaderRunID, runID)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST through the proxy: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp, parsed
}

func newEgressRunID(label string) string {
	return fmt.Sprintf("a2a-%s-%d", label, time.Now().UnixNano())
}

// TestRemoteDelegationIsGovernedAtEgress is A2A-002's acceptance test.
//
// THE HOLE IT CLOSES, raised by Synaptum on 2026-09-20: a governed agent cannot do anything unauthorized
// itself, but it can ASK A THIRD PARTY to. The child's tools cross the child's gateway, not ours, and the
// effect leaves our perimeter at the SendMessage. So the governed thing has to be the delegation.
//
// The form is a DATA-PLANE PROXY, and that was decided against two alternatives I had proposed first: an
// advisory authorize-API (a loop with a bug skips it) and an A2A client of our own (the framework adopts
// our SDK and the task lifecycle moves to us). A control in the framework is a request; a proxy on the
// path is a frontier.
//
// Real throughout: the real checked-in Cedar bundle, a real Postgres registry and ledger, a real Secret
// Broker, and a real remote HTTP server on a real socket.
func TestRemoteDelegationIsGovernedAtEgress(t *testing.T) {
	t.Run("an authorized delegation is proxied, attributed and recorded", func(t *testing.T) {
		f := newEgressFixture(t, 0)
		runID := newEgressRunID("allowed")
		f.remote.set("working", false, "task-allowed")

		resp, body := f.delegate(t, allowedRemote, runID, "message/send", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		// Reached the remote, and reached it with the credential the GATEWAY injected.
		got := f.remote.received()
		if len(got) != 1 {
			t.Fatalf("the remote received %d request(s), want 1", len(got))
		}
		if got[0].authorization != "Bearer s3cr3t-remote-token" {
			t.Errorf("the remote saw Authorization %q — the credential must be injected by the gateway, from the Secret Broker", got[0].authorization)
		}
		// Identity per hop: the remote learns who delegated, not just that a gateway called.
		if got[0].delegatingAgent != egressAgent {
			t.Errorf("delegating agent = %q, want %q", got[0].delegatingAgent, egressAgent)
		}
		if got[0].hopDepth != "1" {
			t.Errorf("hop depth seen by the remote = %q, want 1 — the call we make is itself a hop", got[0].hopDepth)
		}

		// Attributed and recorded.
		rows, err := f.delegations.ForRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("reading the ledger: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("the ledger holds %d row(s) for this run, want 1", len(rows))
		}
		row := rows[0]
		if row.AgentManifestRef != egressAgent || row.RemoteAgentID != allowedRemote || row.RPCMethod != "message/send" {
			t.Errorf("row = %+v, want it attributed to the agent, the destination and the method", row)
		}
		if row.TaskState != "working" {
			t.Errorf("task_state = %q, want the state the remote reported", row.TaskState)
		}
		if row.Terminal == nil || *row.Terminal {
			t.Errorf("terminal = %v, want false — `working` is not a finished task", row.Terminal)
		}
	})

	t.Run("a destination policy does not permit is NOT reached", func(t *testing.T) {
		// The assertion that makes this a frontier rather than a report: the remote server FAILS THE TEST
		// if it is touched. A proxy that denied in its response but forwarded anyway would pass a test that
		// only inspected the status code.
		f := newEgressFixture(t, 0)
		runID := newEgressRunID("denied")

		resp, body := f.delegate(t, "unpermitted-partner", runID, "message/send", nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
		}
		if got := f.remote.received(); len(got) != 0 {
			t.Fatalf("the remote was reached %d time(s) despite the denial — the delegation happened", len(got))
		}
		if allowed, _ := body["allowed"].(bool); allowed {
			t.Error("the response reports the delegation as allowed")
		}

		// Recorded. A refusal that leaves no trace is indistinguishable from an agent that never tried,
		// and "an agent tried to delegate somewhere it may not" is the event worth keeping.
		rows, _ := f.delegations.ForRun(context.Background(), runID)
		if len(rows) != 1 || rows[0].DeniedReason == "" {
			t.Fatalf("the denial was not recorded with a reason: %+v", rows)
		}
		if rows[0].CompletedAt == nil {
			t.Error("the denied row is still in flight — a delegation that never happened must not spend the run's width")
		}
	})

	t.Run("an undeclared destination is refused, and says so differently", func(t *testing.T) {
		// Declared-before-delegable. The message has to differ from the policy refusal above: a missing
		// registry entry and a policy decision are fixed by different people in different places.
		f := newEgressFixture(t, 0)

		resp, body := f.delegate(t, "never-declared-anywhere", newEgressRunID("undeclared"), "message/send", nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
		reason, _ := body["reason"].(string)
		if !bytes.Contains([]byte(reason), []byte("not declared")) {
			t.Errorf("reason = %q, want it to say the destination is not declared rather than that policy refused", reason)
		}
		if got := f.remote.received(); len(got) != 0 {
			t.Fatal("an undeclared destination was contacted")
		}
	})

	t.Run("a delegation with no agent identity is refused, not proxied unattributed", func(t *testing.T) {
		// An unattributed call through this proxy would be a way around the per-agent policy every other
		// path obeys — the failure this endpoint exists to prevent, arriving through the endpoint itself.
		f := newEgressFixture(t, 0)
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{}}`)
		resp, err := http.Post(f.proxy.URL+"/a2a/egress/"+allowedRemote, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
		if got := f.remote.received(); len(got) != 0 {
			t.Fatal("an unattributed delegation reached the remote")
		}
	})

	t.Run("a caller cannot supply its own credential to the destination", func(t *testing.T) {
		// The credential is the gateway's to inject. If a caller-supplied Authorization were merged
		// instead of discarded, a governed process could reach the destination with a credential of its
		// own and the whole arrangement would be decoration.
		f := newEgressFixture(t, 0)
		f.delegate(t, allowedRemote, newEgressRunID("own-cred"), "message/send", map[string]string{
			"Authorization": "Bearer forged-by-the-agent",
		})

		got := f.remote.received()
		if len(got) != 1 {
			t.Fatalf("the remote received %d request(s), want 1", len(got))
		}
		if got[0].authorization == "Bearer forged-by-the-agent" {
			t.Error("the caller's own Authorization reached the destination")
		}
		if got[0].authorization != "Bearer s3cr3t-remote-token" {
			t.Errorf("Authorization = %q, want the broker-issued one", got[0].authorization)
		}
		if !a2a.IsStrippedInbound("Authorization") {
			t.Error("Authorization is not on the strip list")
		}
	})

	t.Run("an unknown remote state is relayed as working, never as terminal", func(t *testing.T) {
		// CONSTRAINT (a), agreed with Synaptum on 2026-09-20. A2A v1.0.1 has an extension mechanism, so
		// states this build has never seen WILL arrive. Closing them loses the remote's real answer in
		// silence — no error anywhere, the parent simply stops waiting.
		//
		// The rewrite in the relayed BODY is what makes the constraint real rather than bookkeeping: our own
		// classification changes nothing for the caller, whose SDK is the thing that would mishandle the
		// string. And the verbatim state must survive somewhere, or the record would claim we understood it.
		f := newEgressFixture(t, 0)
		runID := newEgressRunID("unknown-state")
		f.remote.set("quantum-deliberating", false, "task-unknown")

		resp, body := f.delegate(t, allowedRemote, runID, "message/send", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get(a2a.HeaderTaskTerminal); got != "false" {
			t.Errorf("%s = %q, want false — an unknown state must never be terminal", a2a.HeaderTaskTerminal, got)
		}
		if got := resp.Header.Get(a2a.HeaderTaskState); got != "quantum-deliberating" {
			t.Errorf("%s = %q, want the verbatim state — a rewrite that hides what the remote said is how two sides come to disagree", a2a.HeaderTaskState, got)
		}
		if resp.Header.Get("X-Aeon-Remote-Task-State-Rewritten") != "true" {
			t.Error("the rewrite was not announced: an invisible substitution in a proxy is worse than none")
		}

		result, _ := body["result"].(map[string]any)
		status, _ := result["status"].(map[string]any)
		if got, _ := status["state"].(string); got != "working" {
			t.Errorf("relayed state = %q, want working — the caller's SDK is what would mishandle an unrecognised string", got)
		}

		rows, _ := f.delegations.ForRun(context.Background(), runID)
		if len(rows) != 1 || rows[0].TaskState != "quantum-deliberating" {
			t.Fatalf("the ledger did not keep the verbatim state: %+v", rows)
		}
		if rows[0].CompletedAt != nil {
			t.Error("an unknown-state delegation was closed — it is still outstanding, and closing it would free the run's width for something unfinished")
		}
	})

	for _, state := range []string{"input-required", "auth-required"} {
		t.Run("a remote waiting for a person ("+state+") is not a result", func(t *testing.T) {
			// CONSTRAINT (b). These are an approval on the OTHER SIDE OF THE NETWORK. Relaying them as
			// terminal would make the parent continue with a response the remote never gave — the local
			// analogue of reading our own PAUSED_FOR_APPROVAL as SUCCEEDED.
			f := newEgressFixture(t, 0)
			runID := newEgressRunID("paused")
			f.remote.set(state, false, "task-"+state)

			resp, _ := f.delegate(t, allowedRemote, runID, "message/send", nil)
			if got := resp.Header.Get(a2a.HeaderTaskTerminal); got != "false" {
				t.Errorf("%s = %q, want false", a2a.HeaderTaskTerminal, got)
			}
			if resp.Header.Get(a2a.HeaderPausedForHuman) != "true" {
				t.Errorf("%s not set — 'waiting for a person over there' and 'still working' call for different things from whoever is watching", a2a.HeaderPausedForHuman)
			}
			// These states ARE legal A2A, so the body must pass through untouched.
			if resp.Header.Get("X-Aeon-Remote-Task-State-Rewritten") == "true" {
				t.Error("a legal A2A state was rewritten")
			}

			rows, _ := f.delegations.ForRun(context.Background(), runID)
			if len(rows) != 1 {
				t.Fatalf("ledger rows = %d, want 1", len(rows))
			}
			if rows[0].Terminal == nil || *rows[0].Terminal {
				t.Errorf("terminal = %v, want false", rows[0].Terminal)
			}
			if rows[0].CompletedAt != nil {
				t.Error("a delegation waiting for a person was closed as finished")
			}
		})
	}

	t.Run("a completed remote task closes the delegation", func(t *testing.T) {
		f := newEgressFixture(t, 0)
		runID := newEgressRunID("completed")
		f.remote.set("completed", false, "task-completed")

		resp, _ := f.delegate(t, allowedRemote, runID, "message/send", nil)
		if got := resp.Header.Get(a2a.HeaderTaskTerminal); got != "true" {
			t.Errorf("%s = %q, want true", a2a.HeaderTaskTerminal, got)
		}
		rows, _ := f.delegations.ForRun(context.Background(), runID)
		if len(rows) != 1 || rows[0].CompletedAt == nil {
			t.Fatalf("a completed delegation was not closed: %+v", rows)
		}
	})

	t.Run("a poll that finds the task finished frees the run's width", func(t *testing.T) {
		// The NORMAL way an asynchronous delegation ends, and it arrives as a DIFFERENT HTTP request — so
		// the row cannot be closed by id and is closed by task id instead. Without this, a run's width
		// would stay spent by every task that completed between polls.
		f := newEgressFixture(t, 1)
		runID := newEgressRunID("poll-closes")
		f.remote.set("working", false, "task-polled")

		if resp, body := f.delegate(t, allowedRemote, runID, "message/send", nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("first delegation = %d; %v", resp.StatusCode, body)
		}
		// Width is 1 and one delegation is outstanding, so a second must be refused.
		if resp, _ := f.delegate(t, allowedRemote, runID, "message/send", nil); resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second delegation = %d, want 429 while the first is still working", resp.StatusCode)
		}

		// The remote finishes; a poll observes it. tasks/get does not start work, so it is not itself
		// limited — otherwise a caller could never learn that its budget had freed up.
		f.remote.set("completed", false, "task-polled")
		if resp, body := f.delegate(t, allowedRemote, runID, "tasks/get", nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("poll = %d; %v", resp.StatusCode, body)
		}

		rows, _ := f.delegations.ForRun(context.Background(), runID)
		open := 0
		for _, r := range rows {
			if r.CompletedAt == nil {
				open++
			}
		}
		if open != 0 {
			t.Errorf("%d delegation(s) still in flight after the poll found the task completed", open)
		}
		// And the width is available again.
		f.remote.set("working", false, "task-polled-2")
		if resp, body := f.delegate(t, allowedRemote, runID, "message/send", nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("delegation after the poll = %d, want 200 — the freed width was not reusable; %v", resp.StatusCode, body)
		}
	})

	t.Run("fan-out is bounded, and the bound holds against simultaneous callers", func(t *testing.T) {
		// Synaptum's own words on 2026-09-20: "un agente que delega en cincuenta a la vez no lo para nadie
		// hoy". Depth is theirs — they see the tree. Width is ours — we are on the path of every call.
		//
		// SIMULTANEOUS on purpose. A count-then-insert without the per-run lock lets fifty requests all read
		// zero and all be admitted, which is precisely the case being bounded; a sequential test would pass
		// against that bug.
		const limit, callers = 3, 12
		f := newEgressFixture(t, limit)
		runID := newEgressRunID("fanout")
		f.remote.set("working", false, "")

		var wg sync.WaitGroup
		codes := make([]int, callers)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				body := []byte(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{}}`)
				req, _ := http.NewRequest(http.MethodPost, f.proxy.URL+"/a2a/egress/"+allowedRemote, bytes.NewReader(body))
				req.Header.Set(a2a.HeaderAgentManifestRef, egressAgent)
				req.Header.Set(a2a.HeaderRunID, runID)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Errorf("caller %d: %v", i, err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				codes[i] = resp.StatusCode
			}(i)
		}
		wg.Wait()

		admitted, refused := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				admitted++
			case http.StatusTooManyRequests:
				refused++
			default:
				t.Errorf("unexpected status %d", c)
			}
		}
		if admitted != limit {
			t.Errorf("%d of %d simultaneous delegations were admitted, want exactly %d", admitted, callers, limit)
		}
		if refused != callers-limit {
			t.Errorf("%d refused, want %d", refused, callers-limit)
		}
		// The remote saw only the admitted ones. Refusing in the response while forwarding anyway would
		// leave the blast radius exactly as wide as it was.
		if got := len(f.remote.received()); got != limit {
			t.Errorf("the remote was reached %d time(s), want %d", got, limit)
		}
	})

	t.Run("a run-less caller is governed but not width-limited", func(t *testing.T) {
		// Mode C: a framework with no Aeon run behind it. Still declared, still policy-checked, still
		// recorded — it simply has no width budget to belong to, because putting every run-less caller in
		// one shared bucket would make them throttle each other for no reason.
		f := newEgressFixture(t, 1)
		for i := 0; i < 3; i++ {
			if resp, body := f.delegate(t, allowedRemote, "", "message/send", nil); resp.StatusCode != http.StatusOK {
				t.Fatalf("run-less delegation %d = %d; %v", i, resp.StatusCode, body)
			}
		}
		if got := len(f.remote.received()); got != 3 {
			t.Errorf("the remote was reached %d time(s), want 3", got)
		}
	})

	t.Run("an inline Message reply is terminal; a Task with no state is not", func(t *testing.T) {
		// The pair that keeps "no status" from being one case. A Message means the remote answered inline
		// and there is nothing to poll. A Task with no readable state is something we cannot call finished
		// — and calling it finished would be constraint (a)'s failure through a different door.
		f := newEgressFixture(t, 0)

		f.remote.set("", true, "")
		resp, _ := f.delegate(t, allowedRemote, newEgressRunID("inline"), "message/send", nil)
		if got := resp.Header.Get(a2a.HeaderTaskTerminal); got != "true" {
			t.Errorf("inline Message: %s = %q, want true", a2a.HeaderTaskTerminal, got)
		}

		f.remote.set("", false, "task-no-state")
		resp, _ = f.delegate(t, allowedRemote, newEgressRunID("no-state"), "message/send", nil)
		if got := resp.Header.Get(a2a.HeaderTaskTerminal); got != "false" {
			t.Errorf("Task with no state: %s = %q, want false", a2a.HeaderTaskTerminal, got)
		}
	})

	t.Run("the body reaches the remote byte for byte", func(t *testing.T) {
		// Transparency is what lets a framework point its existing A2A client here and change no code. It
		// also matters for a reason this codebase has already paid for: re-marshalling JSON normalises
		// numbers and key order, which is the divergence the step-identity corpus exists to pin down. A
		// proxy has no need to understand the payload, so it must not touch it.
		f := newEgressFixture(t, 0)
		f.remote.set("working", false, "task-verbatim")

		sent := []byte(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"big":9007199254740993,"z":1,"a":2}}`)
		req, _ := http.NewRequest(http.MethodPost, f.proxy.URL+"/a2a/egress/"+allowedRemote, bytes.NewReader(sent))
		req.Header.Set(a2a.HeaderAgentManifestRef, egressAgent)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		got := f.remote.received()
		if len(got) != 1 {
			t.Fatalf("the remote received %d request(s), want 1", len(got))
		}
		if !bytes.Equal(bytes.TrimSpace(got[0].body), sent) {
			t.Errorf("the body was modified in transit\n got: %s\nwant: %s", got[0].body, sent)
		}
	})
}
