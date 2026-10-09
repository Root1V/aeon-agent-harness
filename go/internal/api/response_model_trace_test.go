package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
)

// renamingFakeProvider answers with a model that is NOT the one it was asked for, which is the case
// the pair exists to detect: a platform that served something else, or a rename in its catalogue.
type renamingFakeProvider struct{ served string }

func (p renamingFakeProvider) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	return map[string]any{
		// Deliberately not renderedContext["model"]. OBS-005's measurement is that this CAN differ,
		// and OBS-008 came from the platform changing what the field meant.
		"model": p.served,
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "hola"},
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	}, nil
}
func (renamingFakeProvider) CachingCapability() string { return "none" }
func (renamingFakeProvider) CostModel() string         { return "token_based" }

// TestADivergentResponseModelIsFindable is OBS-012's acceptance test, against a real OTel Collector
// and a real Tempo.
//
// WHAT THE PAIR IS FOR. `gen_ai.request.model` is what the caller asked for and
// `gen_ai.response.model` is what answered, and they are two attributes because they differ — here
// for two unrelated reasons. By design: a caller asks for a capability PROFILE and never a model
// (docs/adr/0004), so routing and fallback make the two different things, and a silent fall-through
// to a local candidate is the expensive case (MDL-018: local inference is never priced, so a dollar
// ceiling stops capping the moment routing lands there). And because the platform served something
// else: OBS-005's case, and the shape OBS-008 came from.
//
// WHY WE ARE THE RIGHT PLACE TO EMIT IT, which is Argus's own argument turned around. Their P-21
// says the pair is impossible from a gateway, and that is right: there both values come from the
// same already-resolved variable, so they cannot disagree and the pair detects nothing. From a
// client they come from two places — the name this gateway chose, and the name in the response body.
//
// AND THE THIRD ASSERTION IS A MEASUREMENT OF TRACEQL ITSELF, because the comment in
// recordResponseModel makes a claim about what a query language can do and that is not something to
// assert from memory.
func TestADivergentResponseModelIsFindable(t *testing.T) {
	tempoURL := os.Getenv("AEON_TEST_TEMPO_QUERY_URL")
	if tempoURL == "" {
		t.Skip("AEON_TEST_TEMPO_QUERY_URL not set — skipping tracing integration test (see make test-go-integration)")
	}
	flush := ensureTestTracing(t)
	ctx := context.Background()

	requested := fmt.Sprintf("obs012-asked-%d", time.Now().UnixNano())
	served := fmt.Sprintf("obs012-answered-%d", time.Now().UnixNano())

	gw := modelgateway.New()
	gw.RegisterProvider("renaming", renamingFakeProvider{served: served})
	if _, err := gw.Decide(ctx, []modelgateway.Candidate{{Provider: "renaming", Model: requested, Priority: 0}}, map[string]any{}, ""); err != nil {
		t.Fatalf("Gateway.Decide: %v", err)
	}
	// And a second span whose provider reports NO model: the "absent" state that is preserved on
	// purpose, and the one that decides whether `!=` is usable as a query at all.
	silent := fmt.Sprintf("obs012-silent-%d", time.Now().UnixNano())
	gw.RegisterProvider("silent", renamingFakeProvider{served: ""})
	if _, err := gw.Decide(ctx, []modelgateway.Candidate{{Provider: "silent", Model: silent, Priority: 0}}, map[string]any{}, ""); err != nil {
		t.Fatalf("Gateway.Decide (silent): %v", err)
	}

	if err := flush(ctx); err != nil {
		t.Fatalf("flushing spans: %v", err)
	}

	// 1. Both halves of the pair are on the span, and they are DIFFERENT values. One query per
	//    attribute: if the span carried only the request model, the second would never match.
	waitForTempoSpan(t, tempoURL,
		fmt.Sprintf(`{ name = "chat" && span.gen_ai.request.model = "%s" }`, requested), 30*time.Second)
	waitForTempoSpan(t, tempoURL,
		fmt.Sprintf(`{ name = "chat" && span.gen_ai.response.model = "%s" }`, served), 30*time.Second)

	// EVERY QUERY IN THIS TEST IS SCOPED TO A UNIQUE MARKER, and that is not style — it is the only
	// thing that makes a green run mean anything here. Tempo keeps what previous runs sent it, so an
	// UNSCOPED query reads another run's data, including a run of a previous version of this code.
	//
	// That is exactly how this test lied once. It still asserted an `aeon.model.response_differs`
	// flag after the flag had been removed, with no marker on the query, and it PASSED locally
	// against a Tempo that still held the flagged span from the run before the removal. CI's Tempo
	// was clean and said so. The local green was worthless for that one assertion.

	// CONTROL: the silent span has to BE there, or the zeros below prove nothing about the absent
	// state — they would prove the span never arrived.
	waitForTempoSpan(t, tempoURL,
		fmt.Sprintf(`{ name = "chat" && span.gen_ai.request.model = "%s" }`, silent), 30*time.Second)
	if n, err := tempoSearchCount(t, tempoURL, fmt.Sprintf(
		`{ name = "chat" && span.gen_ai.request.model = "%s" && span.gen_ai.response.model != nil }`, silent)); err != nil || n != 0 {
		t.Fatalf("the silent span reports a response.model (matched=%d err=%v) — the ABSENT state is "+
			"not preserved, and defaulting it to the requested model would manufacture agreement", n, err)
	}

	// THE ONE QUERY A CONSUMER ACTUALLY HAS, asserted rather than logged. No prior knowledge of
	// either model name: "did anything today get served by something other than what was asked".
	//
	// It replaced an `aeon.model.response_differs` flag of ours. The comment justifying that flag
	// claimed TraceQL compares an attribute only against a literal — measured false right here, which
	// is why the flag is gone: it was redundant AND a non-standard attribute for Argus's semconv
	// suite to reject.
	divergent := fmt.Sprintf(
		`{ name = "chat" && span.gen_ai.request.model = "%s" && span.gen_ai.request.model != span.gen_ai.response.model }`, requested)
	n, err := tempoSearchCount(t, tempoURL, divergent)
	if err != nil {
		t.Fatalf("Tempo refused the two-attribute comparison (%v). If TraceQL ever stops supporting it, "+
			"a divergence becomes recorded but not findable and this feature needs its own flag back", err)
	}
	if n == 0 {
		t.Fatal("the two-attribute comparison matched nothing for a span whose models genuinely differ")
	}

	// AND THE ABSENT CASE IS NOT A FALSE POSITIVE under that same query, which is what makes it
	// usable: a provider that reports no model would otherwise show up as a divergence every time.
	absentQuery := fmt.Sprintf(
		`{ name = "chat" && span.gen_ai.request.model = "%s" && span.gen_ai.request.model != span.gen_ai.response.model }`, silent)
	if n, err := tempoSearchCount(t, tempoURL, absentQuery); err != nil || n != 0 {
		t.Errorf("the divergence query matches a span with NO response model (matched=%d err=%v) — then "+
			"every provider that reports none reads as a discrepancy", n, err)
	}
}

// tempoSearchCount runs one TraceQL query and reports how many traces matched, or the error Tempo
// answered with. Unlike waitForTempoSpan it does not poll and does not fail: the caller is asking
// what Tempo DOES, not waiting for something to arrive.
func tempoSearchCount(t *testing.T, tempoURL, traceQL string) (int, error) {
	t.Helper()
	reqURL := tempoURL + "/api/search?" + url.Values{"q": {traceQL}, "limit": {"5"}}.Encode()
	resp := getAuthed(t, reqURL)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("Tempo answered %d", resp.StatusCode)
	}
	var parsed tempoSearch
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0, fmt.Errorf("decoding Tempo's answer: %w", err)
	}
	return len(parsed.Traces), nil
}
