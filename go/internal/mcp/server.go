package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aeon-ai/aeon/go/internal/auth"
	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// McpClientPrincipalType is the Cedar entity type an external MCP caller is authorized as. The
// principal ID is the AUTHENTICATED CALLER'S ID since INT-003c — see toolCallHandler.
//
// IT USED TO BE ONE SHARED ID, `external-mcp-client`, with a comment arguing that MCP's `clientInfo`
// is self-reported ("not verified by the protocol... SHOULD NOT [be relied on] for security
// decisions") and so could not be a principal. That part was and remains correct; the conclusion
// drawn from it — that we therefore had no usable identity — was not. We never needed `clientInfo`:
// `/mcp` is mounted behind the same caller authentication as every other route, so the bearer token
// already identifies the caller exactly as it does on `/execute`.
//
// A POLICY THAT NAMED THE OLD ID STOPS MATCHING, which is the one migration cost and is stated here
// because it is the sort of thing that is discovered instead of read: a permit written as
// `principal == McpClient::"external-mcp-client"` now matches nothing. One left unconstrained on the
// principal keeps working. The denial message names the principal that was judged so an operator
// reads which permit is missing rather than guessing.
const McpClientPrincipalType = "McpClient"

// NewToolGatewayServer builds a real MCP server (INT-003 — Modo C from the spec) exposing tools as
// real MCP Tool entries, from Aeon's own governed Tool Registry (TOOL-001). Every tools/call is
// routed through eng.IsAllowedForPrincipal before executor.Execute ever runs — the exact same
// policy-then-execute ordering ToolGatewayHandlers.execute already enforces for native calls (see
// go/internal/api/tool_gateway_handlers.go); this is a second front door onto the same gate, never
// a bypass of it.
func NewToolGatewayServer(tools []*store.ToolRecord, policies *policy.Set, executor ToolExecutor) *sdkmcp.Server {
	server, _ := NewToolGatewayCatalog(tools, policies, executor)
	return server
}

// NewToolGatewayCatalog builds the server AND the catalogue that keeps it fresh (INT-003's live refresh).
//
// The initial load goes through Catalog.Apply, the same path a refresh takes — NOT through a separate
// startup loop. That separate loop is what had the version bug: it called AddTool once per registry row,
// and since List returns every version newest-first, each tool ended up registered with its OLDEST
// schema. One code path means a refresh cannot disagree with a cold start about what the catalogue is.
func NewToolGatewayCatalog(tools []*store.ToolRecord, policies *policy.Set, executor ToolExecutor) (*sdkmcp.Server, *Catalog) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "aeon-toolgw", Version: "0.1.0"}, nil)
	catalog := NewCatalog(server, policies, executor)
	catalog.Apply(tools)
	return server, catalog
}

// toolCallHandler takes the ToolExecutor INTERFACE (see catalog.go) rather than the concrete
// *toolexec.Executor, so the live catalogue can share exactly this handler instead of a near-copy. A
// second handler for the MCP path would be a second place the policy-then-execute ordering has to be kept
// right, which is the one thing INT-003 must not duplicate.
func toolCallHandler(toolName string, policies *policy.Set, executor ToolExecutor) sdkmcp.ToolHandler {
	return func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, fmt.Errorf("mcp: unmarshal arguments for %s: %w", toolName, err)
			}
		}

		// THE REAL CALLER, NOT ONE SHARED PRINCIPAL (INT-003c).
		//
		// `/mcp` sits behind auth.Require like every other route but the probes, so a request that
		// reaches here is ALREADY authenticated against the caller bundle — and measured: the Caller
		// survives into the context the SDK hands this handler. The identity was there all along and
		// this handler threw it away, which is why Veritium found that a per-agent permit does not
		// apply through `/mcp` and had to add a second, blanket one.
		//
		// WHAT THE OLD COMMENT GOT RIGHT AND WHAT IT GOT WRONG. Right: MCP's `clientInfo` is
		// self-reported, the spec says not to rely on it for security, and using it as a principal
		// would invent a trust boundary. Wrong: the conclusion that we therefore had no identity. We
		// are not trusting `clientInfo` here — we are using the bearer token the caller presented,
		// which is the same identity `/execute` has always used. The backlog entry's own entry
		// criterion said this needed SPIFFE first; workload identity is a different and stronger
		// thing, and it was never what stood between us and dropping the shared principal.
		caller, authenticated := auth.CallerFrom(ctx)
		if !authenticated {
			// Unreachable behind auth.Require, and refused rather than defaulted anyway: an absent
			// identity must never become a permissive one. If this ever fires, the mount moved.
			return refusal(fmt.Sprintf("no authenticated caller on an %s call: this server must be mounted behind "+
				"caller authentication", toolName), toolexec.Outcome{}), nil
		}

		// THE CALLER'S OWN TENANT BUNDLE, and resolving it here is not optional once the principal is
		// real. The catalogue used to hold ONE engine, the deployment tenant's, which was coherent
		// while every caller was one principal. With real callers that would authorize a caller of
		// tenant B against tenant A's policies — so the engine comes from the caller's tenant, and a
		// tenant with no bundle is DENIED rather than falling back to somebody else's permits
		// (VRT-AEON-005 T-5).
		eng, hasBundle := policies.EngineFor(caller.Tenant)
		if !hasBundle {
			return refusal(fmt.Sprintf("no policy bundle is loaded for tenant %q, so %s cannot be authorized",
				caller.Tenant, toolName), toolexec.Outcome{}), nil
		}

		decision := eng.IsAllowedForPrincipal(McpClientPrincipalType, caller.ID, toolName)
		if !decision.Allowed {
			// A policy denial is a real, informative outcome for the caller to see and adjust
			// to — not a protocol-level failure. Same reasoning the spec itself gives for
			// tool-level errors: "otherwise the LLM would not be able to see that an error
			// occurred and self-correct."
			// INT-014: THIS DOOR RECORDED NOTHING AT ALL before now — not its executions, which
			// INT-013 fixed, and not its refusals, which the HTTP door has journalled since
			// INT-011. And it is the door with no run behind it, so the run journal was never an
			// option: "qué agente puede llamar cuál" was unanswerable for exactly the callers that
			// are not ours.
			refusal := executor.RecordRefusal(ctx,
				toolexec.Invocation{Tenant: caller.Tenant, ToolName: toolName, Args: args, Door: toolexec.DoorMCP},
				string(decision.Disposition), decision.PolicyID)
			return &sdkmcp.CallToolResult{
				IsError: true,
				Content: []sdkmcp.Content{&sdkmcp.TextContent{
					// The disposition (INT-010) is in the text because that is the only channel an MCP client
					// has: the protocol carries a boolean IsError and nowhere to put "refused, but a person
					// could still approve this". A caller that can only read prose should still be able to
					// tell "try something else" from "stop".
					// THE PRINCIPAL IS NAMED. With one shared principal there was nothing to say; now a
					// refusal is actionable — an operator reads which Cedar principal was judged and
					// knows exactly which permit is missing.
					Text: fmt.Sprintf("denied by policy (%s): %s is not permitted for %s::%q",
						decision.Disposition, toolName, McpClientPrincipalType, caller.ID),
				}},
				Meta: recordingMeta(refusal),
			}, nil
		}

		// THE CALLER'S TENANT, which this door could not supply before INT-003c and so passed the empty
		// string — deliberately, because a tenant-scoped tool refusing is better than one falling back
		// to a deployment-wide store (GOV-001g). That reasoning stands; what changed is that there is
		// now a real tenant to pass, from the authenticated token rather than from the wire. So a
		// tenant-scoped tool reached through `/mcp` now works, scoped to the caller's own tenant,
		// instead of always refusing.
		//
		// Door, and no run attribution: an external MCP caller is not a run, so RunID and
		// StepID stay empty and the bitácora records that honestly instead of inventing one. Before
		// INT-013 this door recorded NOTHING — not the execution, and not even the denial above, which
		// the HTTP door has journalled since INT-011. Which entrance you used decided whether your call
		// existed in the record.
		out, err := executor.Execute(ctx, toolexec.Invocation{
			Tenant: caller.Tenant, ToolName: toolName, Args: args, Door: toolexec.DoorMCP,
		})
		if err != nil {
			return &sdkmcp.CallToolResult{
				IsError: true,
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: err.Error()}},
				Meta:    recordingMeta(out),
			}, nil
		}

		// A FORWARDED RESULT IS PASSED THROUGH, NOT RE-WRAPPED (TOOL-010b). A federated tool
		// (TOOL-010) hands back someone else's protocol result; encoding that into one text block
		// made a client decode twice and threw away the structured shape the upstream tool declared
		// — measured by Veritium against their own server. An ordinary tool's map still gets the
		// encoding below, because that is the only shape it has.
		if content, structured, forwarded := toolexec.ForwardedFrom(out.Result); forwarded {
			blocks, err := contentBlocksFrom(content)
			if err != nil {
				return nil, fmt.Errorf("mcp: forwarding the result of %s: %w", toolName, err)
			}
			return &sdkmcp.CallToolResult{
				Content:           blocks,
				StructuredContent: structured,
				Meta:              recordingMeta(out),
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

// contentBlocksFrom turns forwarded content blocks back into the SDK's types by re-encoding them
// and letting the SDK decode them, which keeps this translation honest about kinds it does not
// know: a block type the spec grows is carried by the SDK's own unmarshalling rather than by a
// switch here that would silently drop it.
func contentBlocksFrom(content []any) ([]sdkmcp.Content, error) {
	if len(content) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(map[string]any{"content": content})
	if err != nil {
		return nil, fmt.Errorf("re-encoding content: %w", err)
	}
	// DECODED THROUGH CallToolResult AND NOT INTO []Content DIRECTLY: Content is a slice of an
	// INTERFACE, so encoding/json cannot construct its elements on its own — it fails with "cannot
	// unmarshal object into .0 of type mcp.Content". The SDK's own result type knows how to pick the
	// concrete block for each kind, which is also what keeps this forwarding correct for a content
	// kind the spec grows later: the SDK learns it, this function does not have to.
	var decoded sdkmcp.CallToolResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decoding content: %w", err)
	}
	return decoded.Content, nil
}

// refusal is a tool-level refusal: the call reached us, we would not run it, and the caller can read
// why. A protocol-level error would say instead that the call itself failed, which the spec is
// explicit about keeping apart — "otherwise the LLM would not be able to see that an error occurred
// and self-correct".
func refusal(text string, out toolexec.Outcome) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{
		IsError: true,
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: text}},
		Meta:    recordingMeta(out),
	}
}
