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
	return func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
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

		// INT-003's external MCP callers share one Cedar principal and are not a run, so there is no
		// run tenant to pass. The empty string is deliberate and the tenant-scoped tools refuse it
		// rather than fall back to a deployment-wide store — an external caller reading artifacts
		// somebody's run produced is exactly what GOV-001g closed.
		// Door, and no run attribution: an external MCP caller is not a run (see above), so RunID and
		// StepID stay empty and the bitácora records that honestly instead of inventing one. Before
		// INT-013 this door recorded NOTHING — not the execution, and not even the denial above, which
		// the HTTP door has journalled since INT-011. Which entrance you used decided whether your call
		// existed in the record.
		out, err := executor.Execute(ctx, toolexec.Invocation{ToolName: toolName, Args: args, Door: toolexec.DoorMCP})
		if err != nil {
			return &sdkmcp.CallToolResult{
				IsError: true,
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: err.Error()}},
				Meta:    recordingMeta(out),
			}, nil
		}

		raw, err := json.Marshal(out.Result)
		if err != nil {
			return nil, fmt.Errorf("mcp: marshal result for %s: %w", toolName, err)
		}
		return &sdkmcp.CallToolResult{
			Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: string(raw)}},
			StructuredContent: out.Result,
			Meta:              recordingMeta(out),
		}, nil
	}
}

// recordingMeta puts INT-013's recording status in `_meta` rather than in the result text.
//
// `_meta` and not the text: the HTTP doors answer `recorded` as a field beside the result, and a
// consumer of this door should be able to read the same fact without parsing prose or having the
// tool's own JSON polluted with gateway bookkeeping. It is the protocol's own channel for exactly
// this — "reserved by the protocol to allow clients and servers to attach additional metadata".
// Absent when the gateway has no bitácora configured, because absent and false are different
// claims: false would say a configured log refused the write.
func recordingMeta(out toolexec.Outcome) map[string]any {
	if out.Recorded {
		return map[string]any{"aeon.tool.invocation_recorded": true}
	}
	if out.RecordErr != nil {
		return map[string]any{
			"aeon.tool.invocation_recorded": false,
			"aeon.tool.record_error":        out.RecordErr.Error(),
		}
	}
	return nil
}
