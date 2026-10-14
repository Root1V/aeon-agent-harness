package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aeon-ai/aeon/go/internal/httpserver"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TestPostRunsLeavesAServerSpanInTheCallersTrace is ARG-AEON-001's acceptance criterion, in their
// words: "una petición a POST /runs que llega con traceparent deja un span de servidor en la misma
// traza, con el padre que venía en la cabecera. Nada más."
//
// WHY A DELIBERATELY INVALID BODY. The span has to exist for a request that ARRIVED, not for one
// that parsed — a server span whose existence depends on the payload cannot account for the time of
// the requests that fail. Sending a body the handler rejects also means this test needs no Temporal:
// it measures the hop, which is what was missing, and not the run.
//
// AND THE SAME-TRACE ASSERTION IS WHAT PROVES THE PARENT CAME FROM THE HEADER. TraceQL's `&&`
// between two span sets means "in the same trace", so anchoring on a client span of ours fails if
// the server span had started a NEW root — which is exactly the defect Argus saw from the other
// side: three subtrees whose parent was nowhere.
func TestPostRunsLeavesAServerSpanInTheCallersTrace(t *testing.T) {
	tempoURL := os.Getenv("AEON_TEST_TEMPO_QUERY_URL")
	if tempoURL == "" {
		t.Skip("AEON_TEST_TEMPO_QUERY_URL not set — skipping tracing integration test (see make test-go-integration)")
	}
	flush := ensureTestTracing(t)

	mux := http.NewServeMux()
	(&RunControllerHandlers{}).Register(mux)
	// THROUGH THE REAL EXTRACTION MIDDLEWARE, outermost, exactly as httpserver.New composes it in
	// every binary. The first version of this test mounted a bare mux and FAILED — measured: the
	// server span existed and the caller's span existed, and the same-trace query returned 0, because
	// nothing had read the traceparent. That is the same shape Argus saw from the other side, and it
	// is why this test covers the two halves together: a span of ours is worth nothing to a waterfall
	// if the context it hangs from was never extracted.
	srv := httptest.NewServer(httpserver.ExtractTraceContext(authWrap(t, mux, "deep-research-general@0.1.0")))
	t.Cleanup(srv.Close)

	caller := fmt.Sprintf("obs017-caller-%d", time.Now().UnixNano())
	ctx, clientSpan := otel.Tracer("obs017-test").Start(context.Background(), caller, trace.WithSpanKind(trace.SpanKindClient))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/runs", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testCallerToken)
	// The header the criterion is about. Injected explicitly rather than relying on an instrumented
	// client, so what travels is visible in this test.
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	if req.Header.Get("traceparent") == "" {
		t.Fatal("no traceparent was injected, so this test would pass without proving anything")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	resp.Body.Close()
	// A rejected body is the case on purpose; what matters is that the hop was accounted for.
	if resp.StatusCode != http.StatusBadRequest {
		t.Logf("status = %d (the criterion is about the span, not the outcome)", resp.StatusCode)
	}
	clientSpan.End()

	if err := flush(context.Background()); err != nil {
		t.Fatalf("flushing spans: %v", err)
	}

	waitForTempoSpan(t, tempoURL, fmt.Sprintf(
		`{ name = "POST /runs" && kind = server } && { name = "%s" }`, caller), 30*time.Second)

	// KIND SERVER AND NOT INTERNAL, asserted separately because every span this codebase emitted was
	// Internal by default — there was no span describing "aeon received a request" in any surface,
	// which is why the criterion names the kind.
	if n, err := tempoSearchCount(t, tempoURL, fmt.Sprintf(
		`{ name = "POST /runs" && kind = internal } && { name = "%s" }`, caller)); err != nil || n != 0 {
		t.Fatalf("the hop span is Internal (matched=%d err=%v): a waterfall needs the server kind to show "+
			"where the request was received", n, err)
	}
}
