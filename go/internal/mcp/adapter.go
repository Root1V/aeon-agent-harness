// Package mcp is Aeon's MCP Adapter (TOOL-002): a client for external MCP tool servers, built
// directly on the real, official github.com/modelcontextprotocol/go-sdk rather than a hand-rolled
// JSON-RPC implementation — the same "use the real SDK" convention as go.temporal.io/sdk,
// go.opentelemetry.io/otel and github.com/jackc/pgx elsewhere in this codebase. Aeon never speaks
// raw MCP wire format itself; this package is a thin translation layer between the SDK's
// Client/ClientSession and whatever inside Aeon needs to reach an external tool server.
//
// SINCE TOOL-010 IT HAS A PRODUCTION CALLER: go/internal/toolsource federates an external MCP
// server's approved tools into the Tool Gateway's catalogue and calls them through this client. This
// doc used to end "the Tool Gateway, eventually", and that word was accurate for long enough that
// VRT-AEON-006 asked whether the capability existed and the answer had to be no.
//
// "core stateless 2026-07-28 + legacy adapter" (roadmap.md's TOOL-002 criterion) is deliberately
// ONE adapter, not two parallel implementations: the underlying SDK's Client.Connect already
// performs the real SEP-2575 negotiation — probing a server with `server/discover`, and falling
// back to the legacy `initialize` handshake (protocol version 2025-11-25) when a server doesn't
// support the new stateless discovery method. See adapter_test.go's two conformance suites, each
// run against a real MCP server (also built on this same SDK), proving both paths for real.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Adapter is Aeon's identity when connecting to any external MCP server — one Adapter can open
// many Sessions (one per server), matching how a single Aeon deployment may reach several
// governed external tool catalogs.
type Adapter struct {
	client *sdkmcp.Client
}

// NewAdapter builds an Adapter identifying itself to every MCP server it connects to.
func NewAdapter() *Adapter {
	return &Adapter{
		client: sdkmcp.NewClient(&sdkmcp.Implementation{Name: "aeon-toolgw", Version: "0.1.0"}, nil),
	}
}

// Session is one connection to a single external MCP server.
type Session struct {
	cs *sdkmcp.ClientSession
}

// Connect dials endpoint — a streamable-HTTP MCP server URL — and negotiates a protocol version.
// The negotiation itself (2026-07-28 stateless if the server supports it, falling back to the
// legacy 2025-11-25 initialize handshake otherwise) happens inside the SDK's Client.Connect; this
// method does not choose or force a version.
func (a *Adapter) Connect(ctx context.Context, endpoint string) (*Session, error) {
	return a.ConnectWithClient(ctx, endpoint, nil)
}

// ConnectWithClient is Connect with the HTTP client the transport should use — which is how
// authentication reaches an external MCP server (TOOL-010). The MCP spec puts authorization in
// ordinary HTTP headers, so an OAuth2 client-credentials source is an http.Client whose transport
// attaches the bearer token, and nothing about the protocol handling changes.
//
// A nil client means the SDK's default, which is correct for a source that needs no credential.
// Passing one is NOT the same as passing a configured token: the client is the thing that refreshes,
// so a long-lived session does not outlive its token.
func (a *Adapter) ConnectWithClient(ctx context.Context, endpoint string, httpClient *http.Client) (*Session, error) {
	transport := &sdkmcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient}
	cs, err := a.client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect to %s: %w", endpoint, err)
	}
	return &Session{cs: cs}, nil
}

// NegotiatedProtocolVersion reports which MCP protocol version this session actually settled on
// (e.g. "2026-07-28", or the legacy "2025-11-25" after a fallback) — surfaced so callers, tests,
// and traces can tell which path a given server actually took, not just that some connection
// succeeded.
func (s *Session) NegotiatedProtocolVersion() string {
	if ir := s.cs.InitializeResult(); ir != nil {
		return ir.ProtocolVersion
	}
	return ""
}

// DiscoveredTool is Aeon's own lightweight view of an external MCP tool — just enough to register
// it as a candidate Aeon ToolDescriptor (TOOL-001). Risk classification and policy for a
// discovered tool are never inferred here; per go/internal/store's ToolRegistry, every tool needs
// an explicit side_effect/risk before it can be registered, and that stays a human/operator
// decision (see roadmap.md's note on this) regardless of where the tool's implementation lives.
type DiscoveredTool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ListTools lists every tool the connected server currently offers.
func (s *Session) ListTools(ctx context.Context) ([]DiscoveredTool, error) {
	result, err := s.cs.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: list tools: %w", err)
	}

	tools := make([]DiscoveredTool, 0, len(result.Tools))
	for _, t := range result.Tools {
		// Per the SDK's own doc comment on Tool.InputSchema: "From the client, this field will
		// hold the default JSON marshaling of the server's input schema (a map[string]any)" — no
		// re-marshal/re-decode needed, just a type assertion.
		schema, _ := t.InputSchema.(map[string]any)
		tools = append(tools, DiscoveredTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return tools, nil
}

// ToolCallOutcome is what Aeon's tool execution path needs back from a call, regardless of
// transport — the same shape a native (non-MCP) tool result already takes.
type ToolCallOutcome struct {
	IsError bool
	Text    string // concatenated text content, if any
	// Content and StructuredContent are the result AS THE PROTOCOL CARRIES IT, and they exist
	// because flattening to Text lost something real. Veritium measured it against TOOL-010: their
	// tool declared structured output, this adapter kept only the concatenated text, the federated
	// executor wrapped that in a map and the MCP door JSON-encoded the map — so a client had to
	// decode twice and the structured shape the tool declared was gone. Forwarding these two makes
	// a federated tool answer the way the source answered.
	//
	// Both are decoded from the protocol's JSON rather than exposing the SDK's types, so a caller
	// of this package does not take a dependency on the SDK to read a result.
	Content           []any
	StructuredContent any
}

// CallTool invokes name on the connected server with args. Per the spec, a tool-level failure
// (the tool ran but reported an error) comes back as IsError=true with the error described in
// Text — a protocol-level failure (unknown tool, bad arguments, transport error) comes back as a
// non-nil error instead. Aeon's caller must not conflate the two: a tool-level error is a result
// to act on; a protocol-level error is this call failing outright.
func (s *Session) CallTool(ctx context.Context, name string, args map[string]any) (*ToolCallOutcome, error) {
	result, err := s.cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("mcp: call tool %s: %w", name, err)
	}

	var text strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	outcome := &ToolCallOutcome{IsError: result.IsError, Text: text.String()}
	// Round-tripped through JSON because Content is a slice of interfaces whose concrete types are
	// the SDK's: re-encoding is what turns them into the plain shapes the protocol defines, without
	// this package having to know every content kind the spec may grow.
	if len(result.Content) > 0 {
		raw, err := json.Marshal(result.Content)
		if err != nil {
			return nil, fmt.Errorf("mcp: re-encoding the content of %s: %w", name, err)
		}
		if err := json.Unmarshal(raw, &outcome.Content); err != nil {
			return nil, fmt.Errorf("mcp: decoding the content of %s: %w", name, err)
		}
	}
	// Left nil when the tool sent none: absent and null are different answers, and a tool with no
	// output schema legitimately sends nothing here.
	if result.StructuredContent != nil {
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return nil, fmt.Errorf("mcp: re-encoding the structured content of %s: %w", name, err)
		}
		if err := json.Unmarshal(raw, &outcome.StructuredContent); err != nil {
			return nil, fmt.Errorf("mcp: decoding the structured content of %s: %w", name, err)
		}
	}
	return outcome, nil
}

// Close ends the session.
func (s *Session) Close() error {
	return s.cs.Close()
}
