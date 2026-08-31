package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoParams struct {
	Message string `json:"message"`
}

// newConformanceServer builds a real MCP server (github.com/modelcontextprotocol/go-sdk) with two
// tools — "echo" (a normal successful call) and "boom" (a tool-level failure, IsError=true) — the
// same server backing both conformance suites below. legacy, when true, forces every
// server/discover response to advertise only protocol version 2025-11-25 (mirroring the SDK's own
// TestInMemory_E2E_DiscoverFallback_NoOverlap test), so a real client is forced through the legacy
// initialize handshake instead of the new stateless discovery path.
func newConformanceServer(t *testing.T, legacy bool) *httptest.Server {
	t.Helper()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "aeon-conformance-server", Version: "0.1.0"}, nil)

	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "Echoes the given message"},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, args echoParams) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "echo: " + args.Message}}}, nil, nil
		},
	)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "boom", Description: "Always reports a tool-level failure"},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				IsError: true,
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "boom: tool failed on purpose"}},
			}, nil, nil
		},
	)

	if legacy {
		server.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
			return func(ctx context.Context, method string, req sdkmcp.Request) (sdkmcp.Result, error) {
				if method == "server/discover" {
					return &sdkmcp.DiscoverResult{
						Meta:              sdkmcp.Meta{sdkmcp.MetaKeyServerInfo: &sdkmcp.Implementation{Name: "aeon-conformance-server-legacy", Version: "0.1.0"}},
						SupportedVersions: []string{"2025-11-25"},
						Capabilities:      &sdkmcp.ServerCapabilities{},
					}, nil
				}
				return next(ctx, method, req)
			}
		})
	}

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{
		// A legacy (pre-2026-07-28) server is necessarily stateful — statelessness is itself a
		// 2026-07-28 capability, per the spec fetched for this feature (blog.modelcontextprotocol.io/
		// posts/2026-07-28). Only the modern conformance server runs stateless.
		Stateless: !legacy,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// TestAdapterStatelessConformance20260728 is TOOL-002's primary acceptance test: Aeon's own
// Adapter, talking to a real MCP server over real HTTP, negotiates the modern stateless protocol,
// lists a real tool with its schema, and calls it successfully.
func TestAdapterStatelessConformance20260728(t *testing.T) {
	srv := newConformanceServer(t, false)
	ctx := context.Background()

	session, err := NewAdapter().Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	if got := session.NegotiatedProtocolVersion(); got != "2026-07-28" {
		t.Errorf("NegotiatedProtocolVersion() = %q, want 2026-07-28", got)
	}

	tools, err := session.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var echoTool *DiscoveredTool
	for i := range tools {
		if tools[i].Name == "echo" {
			echoTool = &tools[i]
		}
	}
	if echoTool == nil {
		t.Fatalf("expected an %q tool among %+v", "echo", tools)
	}
	if echoTool.Description != "Echoes the given message" {
		t.Errorf("echo tool description = %q", echoTool.Description)
	}
	if echoTool.InputSchema == nil {
		t.Error("expected a real input schema, not nil")
	}

	outcome, err := session.CallTool(ctx, "echo", map[string]any{"message": "hello"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if outcome.IsError {
		t.Fatalf("unexpected tool-level error: %+v", outcome)
	}
	if outcome.Text != "echo: hello" {
		t.Errorf("outcome.Text = %q, want %q", outcome.Text, "echo: hello")
	}
}

// TestAdapterLegacyFallback20251125 is the "legacy adapter" half of TOOL-002's criterion: the same
// Adapter, given a real server that only advertises protocol version 2025-11-25 via
// server/discover, correctly falls back to the legacy initialize handshake and is still fully
// usable — no separate code path in adapter.go, the fallback is the SDK's real, tested behavior.
func TestAdapterLegacyFallback20251125(t *testing.T) {
	srv := newConformanceServer(t, true)
	ctx := context.Background()

	session, err := NewAdapter().Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect (legacy fallback): %v", err)
	}
	defer session.Close()

	if got := session.NegotiatedProtocolVersion(); got != "2025-11-25" {
		t.Errorf("NegotiatedProtocolVersion() = %q, want 2025-11-25 (legacy fallback)", got)
	}

	tools, err := session.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools after legacy fallback: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2 (echo, boom)", len(tools))
	}

	outcome, err := session.CallTool(ctx, "echo", map[string]any{"message": "hi from legacy"})
	if err != nil {
		t.Fatalf("CallTool after legacy fallback: %v", err)
	}
	if outcome.Text != "echo: hi from legacy" {
		t.Errorf("outcome.Text = %q", outcome.Text)
	}
}

// TestAdapterCallToolReportsToolLevelErrorWithoutFailingTheCall proves Aeon's Adapter keeps the
// spec's distinction intact: a tool that ran and failed comes back as a normal result with
// IsError=true, not as a Go error — so a caller can see and act on it, per the spec's own
// reasoning ("otherwise the LLM would not be able to see that an error occurred").
func TestAdapterCallToolReportsToolLevelErrorWithoutFailingTheCall(t *testing.T) {
	srv := newConformanceServer(t, false)
	ctx := context.Background()

	session, err := NewAdapter().Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	outcome, err := session.CallTool(ctx, "boom", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool(boom): unexpected protocol-level error: %v", err)
	}
	if !outcome.IsError {
		t.Error("expected IsError=true for a tool that reports its own failure")
	}
	if outcome.Text == "" {
		t.Error("expected a non-empty error message in Text")
	}
}

func TestAdapterCallToolOnUnknownNameIsAProtocolError(t *testing.T) {
	srv := newConformanceServer(t, false)
	ctx := context.Background()

	session, err := NewAdapter().Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	if _, err := session.CallTool(ctx, "not-a-real-tool", map[string]any{}); err == nil {
		t.Error("expected a protocol-level error for an unknown tool name")
	}
}
