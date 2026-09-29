package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
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
func NewToolGatewayServer(tools []*store.ToolRecord, eng *policy.Engine, executor ToolExecutor) *sdkmcp.Server {
	server, _ := NewToolGatewayCatalog(tools, eng, executor)
	return server
}

// NewToolGatewayCatalog builds the server AND the catalogue that keeps it fresh (INT-003's live refresh).
//
// The initial load goes through Catalog.Apply, the same path a refresh takes — NOT through a separate
// startup loop. That separate loop is what had the version bug: it called AddTool once per registry row,
// and since List returns every version newest-first, each tool ended up registered with its OLDEST
// schema. One code path means a refresh cannot disagree with a cold start about what the catalogue is.
func NewToolGatewayCatalog(tools []*store.ToolRecord, eng *policy.Engine, executor ToolExecutor) (*sdkmcp.Server, *Catalog) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "aeon-toolgw", Version: "0.1.0"}, nil)
	catalog := NewCatalog(server, eng, executor)
	catalog.Apply(tools)
	return server, catalog
}

// toolCallHandler takes the ToolExecutor INTERFACE (see catalog.go) rather than the concrete
// *toolexec.Executor, so the live catalogue can share exactly this handler instead of a near-copy. A
// second handler for the MCP path would be a second place the policy-then-execute ordering has to be kept
// right, which is the one thing INT-003 must not duplicate.
func toolCallHandler(toolName string, eng *policy.Engine, executor ToolExecutor) sdkmcp.ToolHandler {
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
					// The disposition (INT-010) is in the text because that is the only channel an MCP client
					// has: the protocol carries a boolean IsError and nowhere to put "refused, but a person
					// could still approve this". A caller that can only read prose should still be able to
					// tell "try something else" from "stop".
					Text: fmt.Sprintf("denied by policy (%s): %s is not permitted for external MCP callers",
						decision.Disposition, toolName),
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
