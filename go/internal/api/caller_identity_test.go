package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/auth"
	"github.com/aeon-ai/aeon/go/internal/httpserver"
	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
	"gopkg.in/yaml.v3"
)

// devCallerBundle loads the bundle the compose stack actually ships with.
//
// THE REAL FILE AND NOT A FIXTURE, which is the point of this test. Every other test in this package
// builds a caller as permissive as its own subject needs; this one asserts what the DEPLOYMENT does, so
// it has to read what the deployment reads. A fixture here would let the committed bundle grow a
// `mayApprove: true` on the worker without a single test noticing.
func devCallerBundle(t *testing.T) *auth.Authenticator {
	t.Helper()
	path := filepath.Join(filepath.Dir(repoPolicyBundlePath(t)), "callers.yaml")
	a, err := auth.LoadFile(path)
	if err != nil {
		t.Fatalf("loading the shipped caller bundle %s: %v", path, err)
	}
	return a
}

// The development tokens, which are public by construction: they are committed in callers.yaml, and
// that file says so. Written here as literals so this test fails if someone rotates them there without
// looking at what depends on them.
const (
	devWorkerToken   = "dev-worker-token-not-a-secret"
	devOperatorToken = "dev-operator-token-not-a-secret"
)

// TestTheDeploymentRefusesCallersItCannotIdentify is SEC-005's acceptance test.
//
// WHAT WAS TRUE BEFORE IT, measured by reading rather than guessed: no Aeon HTTP surface authenticated
// anything. The whole repo's only `Authorization` header was outbound. Whoever reached :9404 could
// start, cancel, pause and APPROVE runs; whoever reached :9403 could execute tools. And the Cedar
// principal came off the wire — `IsAllowed(body.AgentManifestRef, ...)` — so the policy engine was
// default-deny, well tested, and judging whichever identity the caller typed.
//
// So "governed" was true of the engine and not of the deployment, and that gap is the whole difference
// between a harness you can pilot with someone else's data and one you cannot.
func TestTheDeploymentRefusesCallersItCannotIdentify(t *testing.T) {
	engine := loadDevPolicyEngine(t)
	// A real executor with one inert tool registered, for the same reason newTestServer does it: this
	// test is about identity, and a permitted call has to be able to reach something.
	executor := toolexec.NewExecutor()
	executor.Register("search.web", func(_ string, args map[string]any) (map[string]any, error) {
		return map[string]any{"status": "executed"}, nil
	})
	mux := http.NewServeMux()
	(&ToolGatewayHandlers{Policy: policy.SingleTenantSet("default", engine), Executor: executor}).Register(mux)
	// A zero-valued RunControllerHandlers on purpose: every assertion below is a refusal that happens
	// before the controller is reached, and giving it a real one would hide an ordering mistake — a
	// guard that ran AFTER the signal was delivered would still return 403 and the test would pass.
	(&RunControllerHandlers{}).Register(mux)

	// httpserver.New and not a hand-wrapped mux: the probe exemption, the auth wrapper and the order
	// they compose in are part of what is being asserted.
	srv := httptest.NewServer(httpserver.New("aeon-test", mux, devCallerBundle(t)).Handler)
	t.Cleanup(srv.Close)

	t.Run("the probes answer without a credential", func(t *testing.T) {
		// A compose healthcheck and a kubelet probe hold no token. If this broke, every service would
		// come up unhealthy and the fix an operator reaches for first is removing the auth.
		for _, path := range []string{"/healthz", "/readyz"} {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d with no credential, want 200", path, resp.StatusCode)
			}
		}
	})

	t.Run("every other route refuses an unidentified caller", func(t *testing.T) {
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, "/execute"},
			{http.MethodPost, "/runs"},
			{http.MethodPost, "/runs/any-run/approve"},
			{http.MethodPost, "/runs/any-run/reject"},
			{http.MethodPost, "/runs/any-run/cancel"},
			{http.MethodGet, "/runs/any-run"},
		} {
			req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("building %s %s: %v", tc.method, tc.path, err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tc.method, tc.path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s = %d with no credential, want 401. Body: %s", tc.method, tc.path, resp.StatusCode, body)
			}
		}
	})

	t.Run("a caller may only present the agent its entry lists", func(t *testing.T) {
		// `search.web` IS permitted by the shipped policy bundle — for deep-research-general. So a
		// refusal here cannot be Cedar's: the worker is claiming an agent it is not, and the engine
		// never gets asked. That is what makes this assertion about identity and not about policy.
		status, body := postExecuteAs(t, srv, devWorkerToken, "some-other-agent@1.0.0", "search.web")
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 — a caller presented an agent it may not act as", status)
		}
		if body["caller_id"] != "worker" {
			t.Fatalf("the refusal does not name the caller: %v", body)
		}
		errText, _ := body["error"].(string)
		if !strings.Contains(errText, "may not act as") {
			t.Fatalf("the refusal reads as a policy denial rather than an identity one, which sends an "+
				"operator to the wrong file: %q", errText)
		}

		// And its own agent reaches the policy engine, so the guard is not simply refusing everything.
		status, _ = postExecuteAs(t, srv, devWorkerToken, "deep-research-general@0.1.0", "search.web")
		if status == http.StatusForbidden || status == http.StatusUnauthorized {
			t.Fatalf("status = %d for the agent the worker IS allowed to act as — the guard refuses its own caller", status)
		}
	})

	t.Run("a credential good enough to run tools cannot approve", func(t *testing.T) {
		// THE PROPERTY THE WHOLE FEATURE EXISTS FOR. The worker is the process that gets blocked on an
		// approval. If its credential could answer one, the run would approve its own irreversible call
		// and the gate would be a formality that writes an audit entry.
		//
		// No Temporal needed: the refusal happens before the controller is touched, which is also the
		// right order — asking the run about a decision this caller may not make would be wasted work
		// at best and a signal delivered at worst.
		status, body := postApprovalAs(t, srv, devWorkerToken, "any-run", "approve")
		if status != http.StatusForbidden {
			t.Fatalf("the worker's token got %d on /approve, want 403. mayApprove must never be implied "+
				"by mayActAs: %v", status, body)
		}
		if body["caller_id"] != "worker" {
			t.Fatalf("the refusal does not name the caller: %v", body)
		}

		// The positive half — an operator's decision actually landing, with `decided_by` in the journal —
		// is asserted in TestDeniedStepIsJournalledAsKnownOutcome, against a real run really suspended on
		// a real approval. It cannot be asserted here: a nil Controller is all this test needs for the
		// refusals above, because each one happens before the controller is touched.
	})

	t.Run("and the operator cannot execute tools", func(t *testing.T) {
		// The other direction, which matters as much: deciding approvals is not a licence to act as an
		// agent. The operator's entry lists no agents, and an empty mayActAs must permit nothing.
		status, _ := postExecuteAs(t, srv, devOperatorToken, "deep-research-general@0.1.0", "search.web")
		if status != http.StatusForbidden {
			t.Fatalf("status = %d for an operator claiming an agent, want 403", status)
		}
	})
}

func postExecuteAs(t *testing.T, srv *httptest.Server, token, agentManifestRef, toolName string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(toolCallRequest{AgentManifestRef: agentManifestRef, ToolName: toolName, Args: map[string]any{}})
	return doAs(t, srv, token, http.MethodPost, "/execute", raw)
}

func postApprovalAs(t *testing.T, srv *httptest.Server, token, runID, action string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(approvalDecisionRequest{ToolCallHash: "deadbeef"})
	return doAs(t, srv, token, http.MethodPost, "/runs/"+runID+"/"+action, raw)
}

func doAs(t *testing.T, srv *httptest.Server, token, method, path string, raw []byte) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// loadDevPolicyEngine loads the shipped Cedar bundle, so a refusal in this test can be attributed to
// identity rather than to a policy written for the test.
func loadDevPolicyEngine(t *testing.T) *policy.Engine {
	t.Helper()
	raw, err := os.ReadFile(repoPolicyBundlePath(t))
	if err != nil {
		t.Fatalf("reading the policy bundle: %v", err)
	}
	var doc policy.PolicyBundleDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the policy bundle: %v", err)
	}
	engine, err := policy.LoadEngine(doc)
	if err != nil {
		t.Fatalf("loading the policy bundle: %v", err)
	}
	return engine
}
