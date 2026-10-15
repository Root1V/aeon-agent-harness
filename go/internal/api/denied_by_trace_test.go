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

// TestADenialSaysWhoDeniedIt is OBS-019, against real Tempo.
//
// Argus published `argus.denied_by` on 2026-10-02 BECAUSE WE ASKED, and we said we would emit it as
// soon as it existed. They measured it twelve days later over their whole history: seven spans, all
// from their own probes, nobody else. The argument for the attribute was ours too — once
// `argus.guardrail` stopped being set, nothing carried WHO refused.
//
// THE QUERY IS THEIR ALERT'S QUERY: a `denied` outcome with no attribution. Asserting that it finds
// nothing of ours is the only form that means anything, because the attribute is there to make that
// alert quiet for the right reason rather than for lack of data.
func TestADenialSaysWhoDeniedIt(t *testing.T) {
	tempoURL := os.Getenv("AEON_TEST_TEMPO_QUERY_URL")
	if tempoURL == "" {
		t.Skip("AEON_TEST_TEMPO_QUERY_URL not set — skipping tracing integration test (see make test-go-integration)")
	}
	flush := ensureTestTracing(t)

	denied := fmt.Sprintf("obs019-denied-%d", time.Now().UnixNano())
	allowed := fmt.Sprintf("obs019-allowed-%d", time.Now().UnixNano())

	executor := toolexec.NewExecutor()
	for _, name := range []string{denied, allowed} {
		toolName := name
		executor.Register(toolName, func(_ string, _ map[string]any) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		})
	}

	// One permit naming ONE of the two tools, so the denial and the success come from the same
	// bundle and neither can pass by the policy being wrong in a convenient direction.
	eng, err := policy.LoadEngine(policy.PolicyBundleDoc{Policies: []policy.PolicyBundleItem{{
		ID: "obs019", Effect: "permit",
		CedarSource: fmt.Sprintf(`permit(principal, action, resource == Tool::"%s");`, allowed),
	}}})
	if err != nil {
		t.Fatalf("LoadEngine: %v", err)
	}

	mux := http.NewServeMux()
	(&ToolGatewayHandlers{Policy: policy.SingleTenantSet("default", eng), Executor: executor}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux, "deep-research-general@0.1.0"))
	t.Cleanup(srv.Close)

	call := func(tool string) int {
		status, _ := postExecuteBody(t, srv, map[string]any{
			"agent_manifest_ref": "deep-research-general@0.1.0", "tool_name": tool, "args": map[string]any{},
		})
		return status
	}
	if status := call(denied); status != http.StatusForbidden {
		t.Fatalf("the denied tool returned %d, want 403 — this test measures a denial", status)
	}
	if status := call(allowed); status != http.StatusOK {
		t.Fatalf("the permitted tool returned %d, want 200", status)
	}

	if err := flush(context.Background()); err != nil {
		t.Fatalf("flushing spans: %v", err)
	}

	// 1. The denial carries both halves: that it was refused, and by what.
	waitForTempoSpan(t, tempoURL, fmt.Sprintf(
		`{ name = "execute_tool %s" && span.argus.outcome = "denied" && span.argus.denied_by = "policy" }`,
		denied), 30*time.Second)

	// 2. EVERY DENIAL OF THIS RUN IS ATTRIBUTED, which is what Argus's alert is really asking. Said
	//    as two counts rather than as "absent", because `= nil` is NOT valid TraceQL — measured:
	//    Tempo answers 400 to it, while `!= nil` is accepted. So the property is expressed as
	//    "denials" equals "denials that carry an attribution", which is the same claim and is a
	//    query the store will actually run.
	denials, err := tempoSearchCount(t, tempoURL, fmt.Sprintf(
		`{ name = "execute_tool %s" && span.argus.outcome = "denied" }`, denied))
	if err != nil || denials == 0 {
		t.Fatalf("no denial of this run is in the store (matched=%d err=%v)", denials, err)
	}
	attributed, err := tempoSearchCount(t, tempoURL, fmt.Sprintf(
		`{ name = "execute_tool %s" && span.argus.outcome = "denied" && span.argus.denied_by != nil }`, denied))
	if err != nil || attributed != denials {
		t.Fatalf("%d denial(s) of ours and only %d attributed (err=%v) — an unattributed denial is the "+
			"exact span Argus's alert looks for", denials, attributed, err)
	}

	// 3. CONTROL: a success must NOT carry an attribution. `denied_by` on something that was not
	//    denied would make the field meaningless the moment anyone grouped by it.
	waitForTempoSpan(t, tempoURL, fmt.Sprintf(`{ name = "execute_tool %s" }`, allowed), 30*time.Second)
	if n, err := tempoSearchCount(t, tempoURL, fmt.Sprintf(
		`{ name = "execute_tool %s" && span.argus.denied_by != nil }`, allowed)); err != nil || n != 0 {
		t.Fatalf("a permitted call carries argus.denied_by (matched=%d err=%v)", n, err)
	}
}
