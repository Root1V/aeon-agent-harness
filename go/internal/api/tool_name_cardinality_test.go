package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// TestAnUnknownToolNameNeverReachesTheStore is ARG's condition from ARG-AEON-002, measured against
// real Tempo.
//
// THEY ASKED FOR IT AS A CONDITION ON THE RENAME and it is not met today, which is why this test
// exists before `execute_tool {tool_name}`. `gen_ai.tool.name` is set from `body.ToolName` — straight
// off the wire — on a span created BEFORE the policy check and before the tool is resolved. The Tool
// Registry requires only a non-empty name. So any caller can put anything in it, and after the
// rename that anything would also be the SPAN NAME.
//
// ARG measured what that costs on their side: `spanmetrics` emits `span_name` as a label always,
// even for dimensions they do not list, and they have had three incidents this week from exactly
// this shape — a long-poll cursor that would have produced 663 fingerprints, a route template
// carrying a case UUID, and an alert signature carrying Postgres row ids that sent 335
// notifications for one error. Their sentence is the requirement: "lo que agrupa no puede contener
// lo que distingue".
func TestAnUnknownToolNameNeverReachesTheStore(t *testing.T) {
	tempoURL := os.Getenv("AEON_TEST_TEMPO_QUERY_URL")
	if tempoURL == "" {
		t.Skip("AEON_TEST_TEMPO_QUERY_URL not set — skipping tracing integration test (see make test-go-integration)")
	}
	flush := ensureTestTracing(t)

	// Shaped like the real thing ARG is afraid of: a per-case id inside a tool name. Unique so the
	// queries below cannot match another run's spans.
	unbounded := fmt.Sprintf("read_case_%d", time.Now().UnixNano())
	known := "probe.known"

	executor := toolexec.NewExecutor()
	executor.Register(known, func(_ string, _ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})

	mux := http.NewServeMux()
	(&ToolGatewayHandlers{Policy: policy.SingleTenantSet("default", permitAllEngine(t)), Executor: executor}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux, "deep-research-general@0.1.0"))
	t.Cleanup(srv.Close)

	// The call fails — the tool does not exist — and that is the point: an unresolvable name must not
	// be able to mint a span identity on its way to failing.
	if status, _ := postExecuteBody(t, srv, map[string]any{
		"agent_manifest_ref": "deep-research-general@0.1.0",
		"tool_name":          unbounded,
		"args":               map[string]any{},
	}); status == http.StatusOK {
		t.Fatalf("the unknown tool executed (status %d), so this measures nothing", status)
	}
	// A known tool in the same run, as the CONTROL: it proves spans from this run reach Tempo at all,
	// so the absence asserted below cannot be "nothing arrived".
	if status, body := postExecuteBody(t, srv, map[string]any{
		"agent_manifest_ref": "deep-research-general@0.1.0",
		"tool_name":          known,
		"args":               map[string]any{},
	}); status != http.StatusOK {
		t.Fatalf("the known tool failed: %d %v", status, body)
	}

	ctx := context.Background()
	if err := flush(ctx); err != nil {
		t.Fatalf("flushing spans: %v", err)
	}

	waitForTempoSpan(t, tempoURL,
		fmt.Sprintf(`{ span.gen_ai.tool.name = "%s" }`, known), 30*time.Second)

	if n, err := tempoSearchCount(t, tempoURL, fmt.Sprintf(`{ span.gen_ai.tool.name = "%s" }`, unbounded)); err != nil || n != 0 {
		t.Fatalf("a caller-supplied tool name reached the store as gen_ai.tool.name (matched=%d err=%v): %q. "+
			"Every distinct value a caller invents becomes a metric series on Argus's side, and after the "+
			"span rename it becomes a span name too", n, err, unbounded)
	}
	if n, err := tempoSearchCount(t, tempoURL, fmt.Sprintf(`{ name = "execute_tool %s" }`, unbounded)); err != nil || n != 0 {
		t.Fatalf("a caller-supplied tool name became a SPAN NAME (matched=%d err=%v)", n, err)
	}
}
