package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// toolGatewayTestDSN mirrors store.testDSN (unexported there) and memoryTestDSN in
// go/internal/api: this server is only meaningfully tested against a real, Postgres-backed Tool
// Registry (TOOL-001) — the whole point of INT-003 is exposing that real, governed catalog.
func toolGatewayTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("AEON_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("AEON_TEST_PG_DSN not set — skipping Postgres integration test (see make test-go-integration)")
	}
	return dsn
}

const testPolicyBundle = `
permit(
  principal == McpClient::"external-mcp-client",
  action,
  resource
) when {
  ["search.web"].contains(resource.name)
};

forbid(
  principal,
  action,
  resource
) when {
  resource.name like "shell.*"
};
`

// newRealToolGatewayServer registers two real tools in a real Postgres-backed ToolRegistry
// (search.web: permitted for the MCP client principal; shell.exec: explicitly forbidden — same
// shape as examples/deep-research/policy_bundle.yaml), then serves them as a real MCP server
// (this package's NewToolGatewayServer) over real HTTP.
func newRealToolGatewayServer(t *testing.T) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	s, err := store.Connect(ctx, toolGatewayTestDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	registry := s.ToolRegistry()
	searchToolID := "search.web." + uuid.NewString()
	_, err = registry.Create(ctx, map[string]any{
		"tool_id":     searchToolID,
		"name":        "search.web",
		"version":     "1.0.0",
		"description": "Search the web",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required":   []any{"query"},
		},
		"side_effect": "READ_ONLY",
		"risk":        "LOW",
	})
	if err != nil {
		t.Fatalf("register search.web: %v", err)
	}

	shellToolID := "shell.exec." + uuid.NewString()
	_, err = registry.Create(ctx, map[string]any{
		"tool_id":                shellToolID,
		"name":                   "shell.exec",
		"version":                "1.0.0",
		"description":            "Run a shell command",
		"input_schema":           map[string]any{"type": "object"},
		"side_effect":            "WRITE_IRREVERSIBLE",
		"risk":                   "CRITICAL",
		"idempotency_key_fields": []any{"command"},
	})
	if err != nil {
		t.Fatalf("register shell.exec: %v", err)
	}

	all, err := registry.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// A persistent dev Postgres accumulates rows across every prior test run — filter to just the
	// two this test registered rather than asserting anything about the full list's size.
	var tools []*store.ToolRecord
	for _, rec := range all {
		if rec.ToolID == searchToolID || rec.ToolID == shellToolID {
			tools = append(tools, rec)
		}
	}
	if len(tools) != 2 {
		t.Fatalf("expected to find the 2 tools just registered, found %d", len(tools))
	}

	eng, err := policy.LoadEngine(policy.PolicyBundleDoc{Policies: []policy.PolicyBundleItem{{ID: "test", CedarSource: testPolicyBundle}}})
	if err != nil {
		t.Fatalf("LoadEngine: %v", err)
	}

	// This test is about the MCP surface and its policy check, not about searching. It registers its
	// own inert search.web: TOOL-007 removed the stub NewExecutor used to ship, so a deployment with
	// no search provider now gets "unknown tool" rather than a successful-looking empty answer. The
	// double belongs in the test that needs it.
	executor := toolexec.NewExecutor()
	executor.Register("search.web", func(args map[string]any) (map[string]any, error) {
		return map[string]any{"status": "executed", "tool": "search.web", "args": args}, nil
	})

	mcpServer := NewToolGatewayServer(tools, eng, executor)
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return mcpServer }, &sdkmcp.StreamableHTTPOptions{Stateless: true})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// TestToolGatewayServerEndToEndWithRealAdapter is INT-003's acceptance test: a real MCP client
// (this package's own Adapter, TOOL-002) lists and calls Aeon's real, Postgres-backed governed
// tool catalog over real HTTP — "un cliente MCP externo lista y llama tools de Aeon" — proving
// both halves of this package interoperate for real, not just against each side's own mocks.
func TestToolGatewayServerEndToEndWithRealAdapter(t *testing.T) {
	srv := newRealToolGatewayServer(t)
	ctx := context.Background()

	session, err := NewAdapter().Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	discovered, err := session.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	byName := map[string]DiscoveredTool{}
	for _, dt := range discovered {
		byName[dt.Name] = dt
	}
	searchTool, ok := byName["search.web"]
	if !ok {
		t.Fatalf("expected 'search.web' among discovered tools, got %+v", discovered)
	}
	if searchTool.Description != "Search the web" {
		t.Errorf("search.web description = %q, want the real registry description", searchTool.Description)
	}
	if searchTool.InputSchema == nil {
		t.Error("expected the real registered input_schema, got nil")
	}
	if _, ok := byName["shell.exec"]; !ok {
		t.Errorf("expected 'shell.exec' to still be listed — policy governs calls, not visibility")
	}

	// Allowed: the policy bundle permits the MCP client principal to call search.web.
	allowedOutcome, err := session.CallTool(ctx, "search.web", map[string]any{"query": "otters"})
	if err != nil {
		t.Fatalf("CallTool(search.web): %v", err)
	}
	if allowedOutcome.IsError {
		t.Fatalf("search.web: unexpected tool-level error: %+v", allowedOutcome)
	}
	if allowedOutcome.Text == "" {
		t.Error("expected real executor output in Text")
	}

	// Denied: shell.exec is explicitly forbidden — the real Policy Engine must block this before
	// toolexec.Executor.Execute ever runs, not just describe the tool as unavailable.
	deniedOutcome, err := session.CallTool(ctx, "shell.exec", map[string]any{"command": "rm -rf /"})
	if err != nil {
		t.Fatalf("CallTool(shell.exec): unexpected protocol-level error: %v", err)
	}
	if !deniedOutcome.IsError {
		t.Fatal("shell.exec: expected a tool-level error (denied by policy), got success")
	}
	if !strings.Contains(deniedOutcome.Text, "denied by policy") {
		t.Errorf("shell.exec denial text = %q, want it to explain the policy denial", deniedOutcome.Text)
	}
}
