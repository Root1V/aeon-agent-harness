package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// TestSchedulingExternalWorkIsItsOwnAuthorization is VRT-AEON-001's authorization half, over real
// HTTP against the real Cedar engine, the real checked-in policy bundle and the real caller bundle
// the stack deploys.
//
// WHY THIS ENDPOINT ENFORCES SEC-005 WHILE /check-policy DOES NOT, asserted here so the asymmetry is
// a decision and not an accident: for a tool, the enforcement point is /execute, where the gateway
// decides AND executes — a wrong answer to the advisory /check-policy changes nothing. An external
// activity runs on the consumer's own worker, so Aeon cannot be in the data path and THIS answer is
// the decision. A decision taken on a principal the caller merely asserted is exactly what SEC-005
// removed from /execute.
func TestSchedulingExternalWorkIsItsOwnAuthorization(t *testing.T) {
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
		t.Fatalf("loading Cedar: %v", err)
	}

	mux := http.NewServeMux()
	(&ToolGatewayHandlers{Policy: policy.SingleTenantSet("default", engine), Executor: toolexec.NewExecutor()}).Register(mux)
	const agent = "deep-research-general@0.1.0"
	srv := httptest.NewServer(authWrap(t, mux, agent))
	t.Cleanup(srv.Close)

	ask := func(t *testing.T, body map[string]any) (int, map[string]any) {
		t.Helper()
		encoded, _ := json.Marshal(body)
		resp := postJSONAuthed(t, srv.URL+"/check-activity-policy", encoded)
		defer resp.Body.Close()
		var parsed map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&parsed)
		return resp.StatusCode, parsed
	}

	t.Run("an activity named like a permitted tool is refused", func(t *testing.T) {
		status, body := ask(t, map[string]any{
			"agent_manifest_ref": agent, "activity_name": "search.web", "task_queue": "acme-online",
		})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (the question was well formed): %v", status, body)
		}
		if allowed, _ := body["Allowed"].(bool); allowed {
			t.Errorf("Allowed = true for an ExternalActivity named like a permitted tool: %v", body)
		}
	})

	t.Run("the task queue is required, because a name without it would be authorized on any queue", func(t *testing.T) {
		status, _ := ask(t, map[string]any{"agent_manifest_ref": agent, "activity_name": "acme.process_item"})
		if status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 for a missing task_queue", status)
		}
	})

	t.Run("a caller that may not act as the agent is refused, not answered", func(t *testing.T) {
		// The caller bundle below lets this token act as a DIFFERENT agent. Before SEC-005 the
		// principal came off the wire, so every permit in the bundle was reachable by anyone who
		// knew an agent's name — and the name is in the bundle.
		other := httptest.NewServer(authWrap(t, mux, "some-other-agent@1.0.0"))
		t.Cleanup(other.Close)
		encoded, _ := json.Marshal(map[string]any{
			"agent_manifest_ref": agent, "activity_name": "acme.process_item", "task_queue": "q",
		})
		resp := postJSONAuthed(t, other.URL+"/check-activity-policy", encoded)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("no caller at all is refused rather than served", func(t *testing.T) {
		// Reaching the handler with no authenticated caller means the route was mounted without
		// auth.Require. Answering anyway is how an unauthenticated route comes back one wiring
		// mistake at a time, so the handler refuses instead of trusting the body.
		bare := httptest.NewServer(authWrap(t, mux))
		t.Cleanup(bare.Close)
		encoded, _ := json.Marshal(map[string]any{
			"agent_manifest_ref": agent, "activity_name": "acme.process_item", "task_queue": "q",
		})
		resp, err := http.Post(bare.URL+"/check-activity-policy", "application/json", bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("a permitted activity is allowed, and the answer names the policy", func(t *testing.T) {
		// The shipped bundle's test permit (see policy_bundle.yaml) is scoped to an agent that is
		// deliberately NOT in the registry, so this cannot widen any real agent.
		const testAgent = "activity-node-test@0.1.0"
		scoped := httptest.NewServer(authWrap(t, mux, testAgent))
		t.Cleanup(scoped.Close)
		encoded, _ := json.Marshal(map[string]any{
			"agent_manifest_ref": testAgent,
			"activity_name":      "aeon.test.external_activity",
			"task_queue":         "test-external",
		})
		resp := postJSONAuthed(t, scoped.URL+"/check-activity-policy", encoded)
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if allowed, _ := body["Allowed"].(bool); !allowed {
			t.Fatalf("the shipped bundle no longer permits the test activity: %v", body)
		}
		if body["PolicyID"] == "" {
			t.Error("an allow with no policy id: the run's record would not say WHY it could hand work outside")
		}
	})
}
