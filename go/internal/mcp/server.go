package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// McpClientPrincipalType is the Cedar entity type every external MCP caller is authorized as
// (INT-003) — see McpClientPrincipalID for why this isn't per-client.
const McpClientPrincipalType = "McpClient"

// McpClientPrincipalID is the single, shared Cedar principal every call arriving through the MCP
// server is authorized as. Per the 2026-07-28 spec itself, `clientInfo` (an MCP client's
// self-reported name) is "not verified by the protocol... SHOULD NOT [be relied on] for security
// decisions" — so it cannot be used as a Cedar principal id without inventing a trust boundary
// that doesn't exist. Until real MCP client authentication exists (SEC-002's Secret Broker), every
// external MCP caller shares this one identity, and a policy bundle can grant it only what any
// anonymous external caller should be allowed to reach — never more than an operator explicitly
// permits for this exact principal.
const McpClientPrincipalID = "external-mcp-client"

// NewToolGatewayServer builds a real MCP server (INT-003 — Modo C from the spec) exposing tools as
// real MCP Tool entries, from Aeon's own governed Tool Registry (TOOL-001). Every tools/call is
// routed through eng.IsAllowedForPrincipal before executor.Execute ever runs — the exact same
// policy-then-execute ordering ToolGatewayHandlers.execute already enforces for native calls (see
// go/internal/api/tool_gateway_handlers.go); this is a second front door onto the same gate, never
// a bypass of it.
func NewToolGatewayServer(tools []*store.ToolRecord, eng *policy.Engine, executor *toolexec.Executor) *sdkmcp.Server {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "aeon-toolgw", Version: "0.1.0"}, nil)

	for _, rec := range tools {
		schema, _ := rec.Descriptor["input_schema"].(map[string]any)
		if schema == nil {
			// AddTool requires a non-nil, type:"object" input schema — a tool registered without
			// one (shouldn't happen for a real ToolDescriptor, since input_schema is required by
			// tool_descriptor.schema.json, but this stays defensive rather than panicking on a
			// malformed registry row) falls back to "accept anything".
			schema = map[string]any{"type": "object"}
		}
		description, _ := rec.Descriptor["description"].(string)

		server.AddTool(
			&sdkmcp.Tool{Name: rec.Name, Description: description, InputSchema: schema},
			toolCallHandler(rec.Name, eng, executor),
		)
	}
	return server
}

// toolCallHandler builds the raw ToolHandler for one tool name, closing over it so every call
// through this handler is authorized and executed as that specific tool regardless of what the
// wire request itself claims to be for (defense in depth: the handler is keyed by registration,
// not by trusting req.Params.Name to match).
func toolCallHandler(toolName string, eng *policy.Engine, executor *toolexec.Executor) sdkmcp.ToolHandler {
	return func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, fmt.Errorf("mcp: unmarshal arguments for %s: %w", toolName, err)
			}
		}

		decision := eng.IsAllowedForPrincipal(McpClientPrincipalType, McpClientPrincipalID, toolName)
		if !decision.Allowed {
			// A policy denial is a real, informative outcome for the caller to see and adjust
			// to — not a protocol-level failure. Same reasoning the spec itself gives for
			// tool-level errors: "otherwise the LLM would not be able to see that an error
			// occurred and self-correct."
			return &sdkmcp.CallToolResult{
				IsError: true,
				Content: []sdkmcp.Content{&sdkmcp.TextContent{
					Text: fmt.Sprintf("denied by policy: %s is not permitted for external MCP callers", toolName),
				}},
			}, nil
		}

		result, err := executor.Execute(toolName, args)
		if err != nil {
			return &sdkmcp.CallToolResult{
				IsError: true,
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: err.Error()}},
			}, nil
		}

		raw, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("mcp: marshal result for %s: %w", toolName, err)
		}
		return &sdkmcp.CallToolResult{
			Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: string(raw)}},
			StructuredContent: result,
		}, nil
	}
}
