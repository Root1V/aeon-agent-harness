package toolsource

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	aeonmcp "github.com/aeon-ai/aeon/go/internal/mcp"
	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// testSource is a REAL MCP server over real streamable HTTP, with two tools, that RECORDS every call
// it receives.
//
// The recording is not instrumentation for convenience: Veritium's acceptance criterion says a
// denied call "NUNCA llega" to the source, and the only way to assert that is to ask the source what
// it received. A test that checked Aeon returned an error would pass with the entire forwarding path
// deleted.
type testSource struct {
	mu       sync.Mutex
	received []string
	// schema is swapped mid-test to simulate the source changing a tool after approval.
	schema map[string]any
}

func (ts *testSource) calls() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.received...)
}

func (ts *testSource) record(name string) {
	ts.mu.Lock()
	ts.received = append(ts.received, name)
	ts.mu.Unlock()
}

func (ts *testSource) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "veritium-test-source", Version: "0.1.0"}, nil)

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name: "extract_document", Description: "Extracts fields from a document",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"doc_id": map[string]any{"type": "string"}}},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, args map[string]any) (*sdkmcp.CallToolResult, any, error) {
		ts.record("extract_document")
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "extracted"}}}, nil, nil
	})
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name: "validate_case", Description: "Validates a case and returns a verdict",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"case_id": map[string]any{"type": "string"}}},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, args map[string]any) (*sdkmcp.CallToolResult, any, error) {
		ts.record("validate_case")
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "valid"}}}, nil, nil
	})

	srv := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return server },
		&sdkmcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(srv.Close)
	return srv
}

// TestAFederatedMcpSourceIsListedGovernedAndCalled is TOOL-010's acceptance test, and it is
// Veritium's criterion from VRT-AEON-006 verbatim plus the half we added when accepting it:
//
//	"un servidor MCP de prueba con dos tools; aeon lo registra como fuente, tools/list de
//	 aeon-toolgw muestra las dos, un tools/call permitido por Cedar llega al servidor de prueba y
//	 uno denegado nunca llega"
//
// plus: after approval, the source changes one tool's input_schema and Aeon stops serving THAT tool
// while still serving the other.
func TestAFederatedMcpSourceIsListedGovernedAndCalled(t *testing.T) {
	source := &testSource{}
	srv := source.start(t)
	ctx := context.Background()

	// The digests are computed from what the source actually offers, which is the real approval loop:
	// an operator reads the descriptor and pins its digest. Hard-coding them would make this test a
	// test of a constant.
	extractDigest := digestOf(t, srv.URL, "extract_document")
	validateDigest := digestOf(t, srv.URL, "validate_case")

	bundlePath := writeBundle(t, srv.URL, extractDigest, validateDigest)
	bundle, err := Load(bundlePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	executor := toolexec.NewExecutor()
	fed := New(bundle, executor)
	for _, r := range fed.Refresh(ctx) {
		if r.Err != nil {
			t.Fatalf("source %q: %v", r.SourceID, r.Err)
		}
		if len(r.Drifted) != 0 {
			t.Fatalf("source %q reports drift on a freshly pinned descriptor: %v", r.SourceID, r.Drifted)
		}
	}

	rows, err := fed.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("federated rows = %d, want 2: %+v", len(rows), rows)
	}

	// Cedar permits ONE of the two and nothing else. Both halves of the criterion ride on this one
	// bundle, so neither can pass by the policy being wrong in a convenient direction.
	engine := enginePermitting(t, "veritium.extract_document")
	mcpServer, _ := aeonmcp.NewToolGatewayCatalog(rows, engine, executor)
	cs := connect(t, mcpServer)

	t.Run("tools/list of aeon-toolgw shows both tools, namespaced by the source prefix", func(t *testing.T) {
		listed, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		got := map[string]bool{}
		for _, tl := range listed.Tools {
			got[tl.Name] = true
		}
		for _, want := range []string{"veritium.extract_document", "veritium.validate_case"} {
			if !got[want] {
				t.Errorf("%q is not in aeon's catalogue: %v", want, got)
			}
		}
	})

	t.Run("a tools/call Cedar permits reaches the source", func(t *testing.T) {
		before := len(source.calls())
		res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{
			Name: "veritium.extract_document", Arguments: map[string]any{"doc_id": "d-1"},
		})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("the permitted call failed: %+v", res.Content)
		}
		calls := source.calls()
		if len(calls) != before+1 || calls[len(calls)-1] != "extract_document" {
			t.Fatalf("the source received %v, want one more extract_document", calls)
		}
		// THE UNPREFIXED NAME reached the source. The prefix is Aeon's namespace, not part of the
		// tool's identity there, and forwarding the qualified name would make every federated call
		// fail as an unknown tool.
	})

	t.Run("a tools/call Cedar denies NEVER reaches the source", func(t *testing.T) {
		before := source.calls()
		res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{
			Name: "veritium.validate_case", Arguments: map[string]any{"case_id": "c-1"},
		})
		if err != nil {
			t.Fatalf("CallTool returned a protocol error: %v", err)
		}
		if !res.IsError {
			t.Fatal("the denied call succeeded — policy is not in front of the federated path")
		}
		// THE ASSERTION IS ON THE SOURCE'S OWN RECORD, not on our error. An error from us is also what
		// a forwarded-then-failed call looks like; only the source can say nothing arrived.
		if after := source.calls(); len(after) != len(before) {
			t.Fatalf("the source received %v after a DENIED call (was %v) — the call was forwarded and "+
				"the refusal happened somewhere that does not stop it", after, before)
		}
	})

	t.Run("a descriptor that changed after approval stops being served, and only that one", func(t *testing.T) {
		// The source changes extract_document's schema: same name, same description, different input.
		// This is the case the pin exists for — the classification an operator gave describes a schema,
		// and nothing about the protocol announces that it changed.
		drifted := &testSource{}
		drifted.mu.Lock()
		drifted.mu.Unlock()
		srv2 := startWithAlteredSchema(t, drifted)

		bundle2, err := Load(writeBundleAt(t, srv2.URL, extractDigest, validateDigest))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		fed2 := New(bundle2, toolexec.NewExecutor())
		reports := fed2.Refresh(ctx)
		if len(reports) != 1 || reports[0].Err != nil {
			t.Fatalf("refresh: %+v", reports)
		}
		observed, drifting := reports[0].Drifted["extract_document"]
		if !drifting {
			t.Fatal("the changed descriptor was served: the approval covers whatever the source sends today")
		}
		if observed == extractDigest || len(observed) != 64 {
			t.Errorf("the report must carry the OBSERVED digest so a review can end in a paste, got %q", observed)
		}
		// AND ONLY THAT ONE. A source that changes one tool must not take its siblings down with it.
		rows2, err := fed2.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(rows2) != 1 || rows2[0].Name != "veritium.validate_case" {
			t.Fatalf("rows after drift = %+v, want only veritium.validate_case", rows2)
		}
	})
}

// --- helpers -------------------------------------------------------------------------------------

func digestOf(t *testing.T, endpoint, toolName string) string {
	t.Helper()
	session, err := aeonmcp.NewAdapter().Connect(context.Background(), endpoint)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()
	tools, err := session.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tl := range tools {
		if tl.Name == toolName {
			d, err := DescriptorFingerprint(tl.Name, tl.Description, tl.InputSchema)
			if err != nil {
				t.Fatalf("fingerprint: %v", err)
			}
			return d
		}
	}
	t.Fatalf("the source does not offer %q", toolName)
	return ""
}

func writeBundle(t *testing.T, endpoint, extractDigest, validateDigest string) string {
	t.Helper()
	return writeBundleAt(t, endpoint, extractDigest, validateDigest)
}

func writeBundleAt(t *testing.T, endpoint, extractDigest, validateDigest string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool-sources.yaml")
	body := fmt.Sprintf(`kind: ToolSourceBundle
sources:
  - id: veritium
    endpoint: %s
    prefix: veritium.
    tools:
      - name: extract_document
        side_effect: READ_ONLY
        risk: low
        descriptor_sha256: "%s"
      - name: validate_case
        side_effect: READ_ONLY
        risk: low
        descriptor_sha256: "%s"
`, endpoint, extractDigest, validateDigest)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the bundle: %v", err)
	}
	return path
}

// startWithAlteredSchema serves the same two tool NAMES, with extract_document's input schema
// changed. Same name and same description on purpose: the pin has to catch a change the catalogue
// cannot see any other way.
func startWithAlteredSchema(t *testing.T, ts *testSource) *httptest.Server {
	t.Helper()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "veritium-test-source", Version: "0.1.0"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name: "extract_document", Description: "Extracts fields from a document",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			// The new, unreviewed argument.
			"doc_id": map[string]any{"type": "string"}, "delete_after": map[string]any{"type": "boolean"},
		}},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, _ map[string]any) (*sdkmcp.CallToolResult, any, error) {
		ts.record("extract_document")
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "extracted"}}}, nil, nil
	})
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name: "validate_case", Description: "Validates a case and returns a verdict",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"case_id": map[string]any{"type": "string"}}},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, _ map[string]any) (*sdkmcp.CallToolResult, any, error) {
		ts.record("validate_case")
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "valid"}}}, nil, nil
	})
	srv := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return server },
		&sdkmcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(srv.Close)
	return srv
}

func enginePermitting(t *testing.T, toolName string) *policy.Engine {
	t.Helper()
	eng, err := policy.LoadEngine(policy.PolicyBundleDoc{Policies: []policy.PolicyBundleItem{{
		ID: "tool-010-acceptance", Effect: "permit",
		CedarSource: fmt.Sprintf(`permit(principal, action, resource == Tool::"%s");`, toolName),
	}}})
	if err != nil {
		t.Fatalf("LoadEngine: %v", err)
	}
	return eng
}

func connect(t *testing.T, server *sdkmcp.Server) *sdkmcp.ClientSession {
	t.Helper()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "tool-010-test", Version: "0"}, nil).
		Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

var _ = store.ToolRecord{}
