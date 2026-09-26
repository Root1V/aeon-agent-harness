package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// repoPolicyBundlePath resolves examples/deep-research/policy_bundle.yaml relative to this test
// file, so the test exercises the actual checked-in config-as-code file (FND-003) rather than an
// inline copy that could drift from what aeon-toolgw really loads in the compose stack.
func repoPolicyBundlePath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// this file: go/internal/api/tool_gateway_handlers_test.go -> repo root is four levels up.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	return filepath.Join(repoRoot, "examples", "deep-research", "policy_bundle.yaml")
}

func newTestServer(t *testing.T) *httptest.Server {
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

	// This test is about POLICY, not about searching: it proves a permitted tool reaches the executor
	// and a denied one never does. So it registers its own inert search.web rather than relying on
	// one being there — TOOL-007 removed the stub NewExecutor used to ship, precisely so that a
	// deployment with no search provider cannot answer a search call. The double belongs here, in
	// the test that needs it, and not in the binary.
	executor := toolexec.NewExecutor()
	executor.Register("search.web", func(args map[string]any) (map[string]any, error) {
		return map[string]any{"status": "executed", "tool": "search.web", "args": args}, nil
	})

	mux := http.NewServeMux()
	handlers := &ToolGatewayHandlers{Policy: engine, Executor: executor}
	handlers.Register(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postExecute(t *testing.T, srv *httptest.Server, agentManifestRef, toolName string) (status int, body map[string]any) {
	t.Helper()
	reqBody, _ := json.Marshal(toolCallRequest{AgentManifestRef: agentManifestRef, ToolName: toolName, Args: map[string]any{}})
	resp, err := http.Post(srv.URL+"/execute", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /execute: %v", err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, parsed
}

// TestToolPolicyDeniesOutOfManifestToolCall is SEC-001/TOOL-001's full acceptance test (see
// roadmap.md), exercised over real HTTP against the real handlers, the real Cedar engine, and the
// real checked-in examples/deep-research/policy_bundle.yaml — the same file aeon-toolgw loads in
// the compose stack. It proves the spec's acceptance criterion: "un agente no puede invocar una
// herramienta fuera de su ToolDescriptor/policy aunque el modelo la solicite."
func TestToolPolicyDeniesOutOfManifestToolCall(t *testing.T) {
	srv := newTestServer(t)
	agent := "deep-research-general@0.1.0"

	t.Run("allowed tool executes", func(t *testing.T) {
		status, body := postExecute(t, srv, agent, "search.web")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", status, body)
		}
		if allowed, _ := body["allowed"].(bool); !allowed {
			t.Fatalf("allowed = %v, want true", body["allowed"])
		}
		result, ok := body["result"].(map[string]any)
		if !ok || result["status"] != "executed" {
			t.Fatalf("expected the tool to actually execute, got result=%v", body["result"])
		}
	})

	t.Run("out-of-manifest tool is denied and never executes", func(t *testing.T) {
		status, body := postExecute(t, srv, agent, "shell.exec")
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", status, body)
		}
		if allowed, _ := body["allowed"].(bool); allowed {
			t.Fatalf("allowed = %v, want false", body["allowed"])
		}
		if _, hasResult := body["result"]; hasResult {
			t.Fatalf("a denied call must not carry an execution result, got %v", body["result"])
		}
	})

	t.Run("tool absent from every permit is denied by default", func(t *testing.T) {
		status, _ := postExecute(t, srv, agent, "external.write.database")
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
	})

	t.Run("policy is per-agent, not global", func(t *testing.T) {
		status, _ := postExecute(t, srv, "some-other-agent@1.0.0", "search.web")
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 — an agent with no matching permit policy must be denied", status)
		}
	})
}
