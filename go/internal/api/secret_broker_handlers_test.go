package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/secrets"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// secretsTestPolicyBundle permits only "secrets.whoami" for a single test agent — an inline Cedar
// policy, not the checked-in examples/deep-research/policy_bundle.yaml, since "secrets.whoami" is
// SEC-002's own demonstration tool, not part of any real agent's manifest.
const secretsTestPolicyBundle = `
permit(
  principal == Agent::"secrets-test-agent@1.0.0",
  action,
  resource
) when {
  ["secrets.whoami"].contains(resource.name)
};
`

func newSecretBrokerTestServer(t *testing.T, secretValues map[string]string) (*httptest.Server, *secrets.Broker) {
	t.Helper()
	var doc policy.PolicyBundleDoc
	doc.Policies = []policy.PolicyBundleItem{{ID: "allow-secrets-whoami", Effect: "permit", CedarSource: secretsTestPolicyBundle}}
	engine, err := policy.LoadEngine(doc)
	if err != nil {
		t.Fatalf("loading Cedar engine: %v", err)
	}

	broker := secrets.NewBroker(secretValues)
	executor := toolexec.NewExecutor()
	toolexec.RegisterSecretsTool(executor, broker)

	mux := http.NewServeMux()
	(&ToolGatewayHandlers{Policy: engine, Executor: executor}).Register(mux)
	(&SecretBrokerHandlers{Broker: broker}).Register(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, broker
}

func postIssue(t *testing.T, srv *httptest.Server, name string) (status int, body map[string]any) {
	t.Helper()
	reqBody, _ := json.Marshal(issueLeaseRequest{Name: name})
	resp, err := http.Post(srv.URL+"/secrets/issue", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /secrets/issue: %v", err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, parsed
}

// TestNoSecretInPrompt is SEC-002's acceptance test: a real secret value is issued as a short-lived,
// opaque lease over real HTTP, then a real tool call resolves that lease server-side — and the
// entire round trip's response bodies (exactly what would flow back toward a caller, and from there
// potentially into a rendered model context) are searched byte-for-byte for the raw secret value.
// It must never appear anywhere.
func TestNoSecretInPrompt(t *testing.T) {
	const rawSecret = "sk-do-not-leak-this-9f3a7c2b1e"
	srv, _ := newSecretBrokerTestServer(t, map[string]string{"demo": rawSecret})
	agent := "secrets-test-agent@1.0.0"

	issueStatus, issueBody := postIssue(t, srv, "demo")
	if issueStatus != http.StatusOK {
		t.Fatalf("POST /secrets/issue: status = %d, body = %v", issueStatus, issueBody)
	}
	ref, ok := issueBody["ref"].(string)
	if !ok || ref == "" {
		t.Fatalf("expected a non-empty lease ref, got %v", issueBody)
	}
	if strings.Contains(ref, rawSecret) {
		t.Fatalf("the issued lease reference must never contain the raw secret, got %q", ref)
	}

	execReqBody, _ := json.Marshal(toolCallRequest{
		AgentManifestRef: agent,
		ToolName:         "secrets.whoami",
		Args:             map[string]any{"secret_ref": ref},
	})
	resp, err := http.Post(srv.URL+"/execute", "application/json", bytes.NewReader(execReqBody))
	if err != nil {
		t.Fatalf("POST /execute: %v", err)
	}
	defer resp.Body.Close()
	rawResponseBody := new(bytes.Buffer)
	if _, err := rawResponseBody.ReadFrom(resp.Body); err != nil {
		t.Fatalf("reading response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /execute: status = %d, body = %s", resp.StatusCode, rawResponseBody.String())
	}

	// The actual acceptance check: the raw secret must not appear anywhere in what the Tool Gateway
	// sent back — this is exactly the content that would otherwise flow into a rendered model
	// context if a caller naively forwarded a tool's result.
	if strings.Contains(rawResponseBody.String(), rawSecret) {
		t.Fatalf("the raw secret leaked into the /execute response: %s", rawResponseBody.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rawResponseBody.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v; body = %s", err, rawResponseBody.String())
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result object, got %v", body)
	}
	if result["status"] != "executed" {
		t.Fatalf("expected the tool to actually execute, got result=%v", result)
	}

	// Proves real resolution happened — not a no-op that merely avoided leaking anything by doing
	// nothing: the fingerprint must match a locally computed hash of the known raw secret.
	wantSum := sha256.Sum256([]byte(rawSecret))
	wantFingerprint := hex.EncodeToString(wantSum[:8])
	if result["secret_fingerprint"] != wantFingerprint {
		t.Fatalf("secret_fingerprint = %v, want %q (computed from the real secret) — the tool never actually resolved it", result["secret_fingerprint"], wantFingerprint)
	}
}

func TestSecretBrokerIssueRejectsUnknownSecretName(t *testing.T) {
	srv, _ := newSecretBrokerTestServer(t, map[string]string{"demo": "value"})
	status, _ := postIssue(t, srv, "does-not-exist")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown secret name", status)
	}
}

func TestSecretsWhoamiDeniedByPolicyForAnotherAgent(t *testing.T) {
	srv, _ := newSecretBrokerTestServer(t, map[string]string{"demo": "value"})
	status, body := postExecute(t, srv, "some-other-agent@1.0.0", "secrets.whoami")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — an agent with no matching permit policy must be denied; body=%v", status, body)
	}
}
