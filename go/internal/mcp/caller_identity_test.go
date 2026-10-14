package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aeon-ai/aeon/go/internal/auth"
	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// TestMcpCallsAreGovernedPerCaller is INT-003c's acceptance test.
//
// WHAT IT REPLACES. Every call through `/mcp` used to be authorized as one shared Cedar principal,
// `McpClient::"external-mcp-client"`, on the argument that MCP's self-reported `clientInfo` cannot
// be a security identity. That argument is correct and was never the whole picture: `/mcp` is
// mounted behind the same caller authentication as every other route, so the bearer token already
// identifies the caller exactly as it does on `/execute`. Veritium found the consequence from the
// outside — a per-agent permit does not apply through `/mcp` — and had to add a blanket permit for
// every MCP client to get anything done (their finding 2 in AEON-VRT-002).
//
// THE TENANT IS ASSERTED TOO, because it comes from the same identity and was the other half thrown
// away: this door used to execute with an empty tenant, so a tenant-scoped tool always refused.
func TestMcpCallsAreGovernedPerCaller(t *testing.T) {
	const (
		permittedCaller = "veritium-reader"
		deniedCaller    = "veritium-writer"
		tenant          = "veritium"
		toolName        = "probe.read"
	)

	ran := &tenantRecorder{}
	executor := toolexec.NewExecutor()
	executor.Register(toolName, func(callTenant string, _ map[string]any) (map[string]any, error) {
		ran.record(callTenant)
		return map[string]any{"ok": true}, nil
	})

	// ONE permit, naming ONE caller. Both callers ask for the same tool, so nothing here can pass by
	// the policy being permissive in a convenient direction.
	eng, err := policy.LoadEngine(policy.PolicyBundleDoc{Policies: []policy.PolicyBundleItem{{
		ID: "int-003c", Effect: "permit",
		CedarSource: fmt.Sprintf(`permit(principal == %s::"%s", action, resource == Tool::"%s");`,
			McpClientPrincipalType, permittedCaller, toolName),
	}}})
	if err != nil {
		t.Fatalf("LoadEngine: %v", err)
	}

	server, _ := NewToolGatewayCatalog(
		[]*store.ToolRecord{{
			ToolID: toolName, Version: "1.0.0", Name: toolName, CreatedAt: time.Now(),
			Descriptor: map[string]any{"description": "probe", "input_schema": map[string]any{"type": "object"}},
		}},
		policy.SingleTenantSet(tenant, eng), executor)

	srv := mountWithCallers(t, server, map[string]string{
		permittedCaller: "token-" + permittedCaller,
		deniedCaller:    "token-" + deniedCaller,
	}, tenant)

	t.Run("the caller Cedar names is allowed, and runs under its OWN tenant", func(t *testing.T) {
		res := callAs(t, srv, permittedCaller)
		if res.IsError {
			t.Fatalf("the permitted caller was refused: %+v", res.Content)
		}
		// THE OTHER HALF OF THE SAME IDENTITY. Before this, the door passed an empty tenant on
		// purpose — a tenant-scoped tool refusing beat one reading a deployment-wide store
		// (GOV-001g). Now there is a real tenant to pass, and it comes from the token rather than
		// from the wire.
		if got := ran.tenants(); len(got) != 1 || got[0] != tenant {
			t.Fatalf("the tool ran with tenant(s) %v, want exactly [%q] — an empty tenant means the "+
				"caller's identity is still being discarded at this door", got, tenant)
		}
	})

	t.Run("a DIFFERENT authenticated caller is denied for the same tool", func(t *testing.T) {
		before := len(ran.tenants())
		res := callAs(t, srv, deniedCaller)
		if !res.IsError {
			t.Fatal("a caller with no permit was allowed: every MCP caller is still one principal")
		}
		// NEVER REACHED THE TOOL. Asserted on the tool's own record, not on the error we returned —
		// an error is also what a forwarded-then-failed call looks like.
		if after := len(ran.tenants()); after != before {
			t.Fatalf("the denied call reached the tool (%d -> %d runs)", before, after)
		}
		// THE PRINCIPAL IS NAMED in the refusal, which is what makes it actionable: with one shared
		// principal there was nothing to say, and now an operator reads which permit is missing.
		text := textOf(res)
		if !strings.Contains(text, deniedCaller) {
			t.Errorf("the refusal does not name the principal it judged: %q", text)
		}
	})

	t.Run("a caller whose tenant has no bundle is denied, never served by another tenant's", func(t *testing.T) {
		// Same server, so the ONLY loaded bundle belongs to `tenant`. This caller authenticates fine
		// and belongs somewhere else — VRT-AEON-005 T-5: a tenant with no bundle is denied rather
		// than falling through to somebody else's permits.
		other := mountWithCallers(t, server, map[string]string{permittedCaller: "token-" + permittedCaller}, "some-other-tenant")
		res := callAs(t, other, permittedCaller)
		if !res.IsError {
			t.Fatal("a caller from a tenant with no policy bundle was allowed — it was judged by another tenant's policies")
		}
		if text := textOf(res); !strings.Contains(text, "some-other-tenant") {
			t.Errorf("the refusal does not name the tenant that has no bundle: %q", text)
		}
	})
}

// --- helpers -------------------------------------------------------------------------------------

type tenantRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *tenantRecorder) record(tenant string) {
	r.mu.Lock()
	r.seen = append(r.seen, tenant)
	r.mu.Unlock()
}

func (r *tenantRecorder) tenants() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// mountWithCallers serves the MCP surface behind real caller authentication for several callers, all
// in one tenant.
func mountWithCallers(t *testing.T, server *sdkmcp.Server, tokensByID map[string]string, tenant string) *httptest.Server {
	t.Helper()
	callers := make([]auth.Caller, 0, len(tokensByID))
	for id, token := range tokensByID {
		callers = append(callers, auth.Caller{
			ID: id, Kind: auth.KindService, Tenant: tenant, TokenSHA256: auth.HashToken(token),
		})
	}
	a, err := auth.Load(auth.CallerBundleDoc{Kind: "CallerBundle", Callers: callers})
	if err != nil {
		t.Fatalf("auth.Load: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true}))
	srv := httptest.NewServer(auth.Require(a)(mux))
	t.Cleanup(srv.Close)
	return srv
}

func callAs(t *testing.T, srv *httptest.Server, callerID string) *sdkmcp.CallToolResult {
	t.Helper()
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "int-003c-test", Version: "0"}, nil).
		Connect(context.Background(), &sdkmcp.StreamableClientTransport{
			Endpoint:   srv.URL + "/mcp",
			HTTPClient: &http.Client{Transport: bearerRoundTripper{token: "token-" + callerID}},
		}, nil)
	if err != nil {
		t.Fatalf("connect as %s: %v", callerID, err)
	}
	defer cs.Close()

	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "probe.read", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool as %s: %v", callerID, err)
	}
	return res
}

func textOf(res *sdkmcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
