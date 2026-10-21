package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	openaicompatible "github.com/aeon-ai/aeon/go/internal/providers/openai_compatible"
)

// VRT-AEON-008, their acceptance criterion verbatim: *«un proveedor de prueba responde un tool call con
// `arguments: "{\"a\": "` y `finish_reason: "length"`. El endpoint responde 200 con esa misma cadena y
// `finish_reason: "length"`, y el ledger registra la llamada con su costo.»*
//
// THE REAL ADAPTER AGAINST A REAL HTTP UPSTREAM, not a fake provider. The defect lived inside
// openai_compatible.Decide — it decoded the arguments and returned an error, which the gateway counted
// as a failed candidate and the facade reported as 502. A fake provider registered directly on the
// gateway skips exactly the code that was wrong, so it would have passed before this change too.
//
// What Veritium measured in production: gpt-oss entered the newline loop of VRT-69, hit the token
// ceiling, and the tool arguments came back truncated. Through the direct route that turn REACHES the
// caller, who retries with another temperature because it can see `finish_reason: length`. Through our
// gateway it became 502, Synaptum retried it as a transient error with the same request, and the loop
// repeated. 3 of 3 extractions failed through the governed route, 3 of 3 passed through the direct one.
func TestMalformedToolArgumentsReachTheCallerInsteadOf502(t *testing.T) {
	ctx := context.Background()
	s := newAPITestStore(t)
	ledger := s.FinOpsLedgerFor("default")

	// Their criterion's string is `{"a": `. This is the same failure carrying the content that makes the
	// second half of their report a personal-data defect and not cosmetics: the real case was an
	// extraction of a payslip, truncated mid-value, with the holder's name already in the bytes. Written
	// once here, then used for both assertions — that it comes out byte-identical, and that it appears
	// NOWHERE else in the response.
	const brokenArgs = `{"employee_name": "Ana Quispe`

	model := "toolargs-" + randSuffix(t)
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
		  "model": %q,
		  "choices": [{
		    "index": 0,
		    "finish_reason": "length",
		    "message": {
		      "role": "assistant",
		      "content": null,
		      "tool_calls": [{
		        "index": 0, "id": "call_0", "type": "function",
		        "function": {"name": "submit", "arguments": %q}
		      }]
		    }
		  }],
		  "usage": {"prompt_tokens": 1000, "completion_tokens": 500}
		}`, model, brokenArgs)
	}))
	t.Cleanup(upstream.Close)

	gw := modelgateway.New()
	gw.RegisterProvider("upstream-under-test", &openaicompatible.Adapter{BaseURL: upstream.URL})
	// token_based so the ledger row carries a real number: with a compute_based rate `cost_usd` is NULL,
	// which is the honest value for "unpriced" and would make "registers the call with its cost"
	// unverifiable here.
	pricing := finops.NewPricingTable([]finops.Rate{{
		Provider: "upstream-under-test", Model: model, CostModel: "token_based",
		InputPerMillionUSD: 1, OutputPerMillionUSD: 2,
	}})
	governance := &ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: s, Agents: s}
	mux := http.NewServeMux()
	governance.Register(mux)
	(&OpenAICompatibleHandlers{
		Gateway: gw, Bundle: toolArgsTestBundle(model), Governance: governance,
	}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)

	runID := "toolargs-run-" + randSuffix(t)
	status, body := postChatCompletionWithHeaders(t, srv, map[string]any{
		"model":    "toolargs-test",
		"messages": []any{map[string]any{"role": "user", "content": "extract this"}},
	}, map[string]string{RunIDHeader: runID})

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 — a model turn with unparseable tool arguments is a model outcome "+
			"and not a transport failure, and 502 makes the caller retry a request that will never work: %v",
			status, body)
	}
	choice := firstChoice(t, body)
	if got, _ := choice["finish_reason"].(string); got != "length" {
		t.Errorf("finish_reason = %q, want length — it is the whole reason the caller can tell a "+
			"truncation from bad JSON and retry differently", got)
	}

	message, _ := choice["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("the response carries %d tool calls, want 1: %v", len(calls), message)
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if got, _ := fn["arguments"].(string); got != brokenArgs {
		t.Errorf("arguments = %q, want %q byte-identical — on this wire the field is a string precisely "+
			"so the executor decides, and re-encoding or substituting is deciding for it", got, brokenArgs)
	}
	if got, _ := fn["name"].(string); got != "submit" {
		t.Errorf("the tool name did not survive: %q", got)
	}
	// And the reason travels WITH the call, so a consumer does not have to re-parse the string to learn
	// that it will not parse.
	why, _ := call["aeon_arguments_error"].(string)
	if why == "" {
		t.Error("the call carries no aeon_arguments_error, so a consumer has to re-parse the arguments " +
			"to discover what we already knew")
	}

	// THE PERSONAL-DATA HALF (VRT-58). The error used to carry `%q` of the whole argument string, so a
	// truncated payslip put the holder's name into the 502 body, the caller's logs and both sides'
	// traces. Their sentence is the one that settles it: a name cannot be caught by a regular
	// expression, so a redactor downstream cannot clean this up — the content must never enter the
	// message.
	//
	// COUNTED OVER THE WHOLE RESPONSE AND NOT OVER THE ERROR FIELD, and the count is EXACTLY ONE: the
	// content belongs in `function.arguments`, where the caller asked for it, and nowhere else. A leak
	// into any other field has the same consequence, and an assertion that only looked at the error
	// would miss the next one.
	raw, _ := json.Marshal(body)
	serialized := string(raw)
	for _, fragment := range []string{"employee_name", "Ana", "Quispe"} {
		if n := strings.Count(serialized, fragment); n != 1 {
			t.Errorf("%q appears %d times in the response, want exactly 1 (inside function.arguments): %s",
				fragment, n, serialized)
		}
	}
	// Positively: what the message DOES say is what an operator acts on. Without length and offset the
	// redacted message would be unactionable, which is the other way to get this wrong.
	for _, want := range []string{fmt.Sprintf("%d bytes", len(brokenArgs)), "malformed JSON at byte"} {
		if !strings.Contains(why, want) {
			t.Errorf("the error says %q, want it to name %q", why, want)
		}
	}

	// And the call is billed. Before this change the gateway returned 502 with no ledger row, so the
	// tokens the model had already spent were invisible: paid for and unrecorded.
	spend, err := ledger.SpendForRun(ctx, runID)
	if err != nil {
		t.Fatalf("SpendForRun: %v", err)
	}
	if spend.ModelCalls != 1 || spend.PricedCalls != 1 {
		t.Fatalf("the ledger holds %d calls (%d priced) for the run, want 1 priced — a turn we delivered "+
			"and did not bill is spend nobody can see", spend.ModelCalls, spend.PricedCalls)
	}
	// 1000 input at $1/M + 500 output at $2/M.
	if want := 1000.0/1e6*1 + 500.0/1e6*2; spend.CostUSD != want {
		t.Errorf("cost_usd = %v, want %v", spend.CostUSD, want)
	}
	if upstreamCalls != 1 {
		t.Errorf("the upstream was called %d times, want 1 — a candidate that is retried on a model "+
			"outcome pays twice for the same answer", upstreamCalls)
	}
}

func toolArgsTestBundle(model string) modelgateway.ModelPolicyBundleDoc {
	return modelgateway.ModelPolicyBundleDoc{Profiles: []modelgateway.ModelProfileDoc{{
		Profile: "toolargs-test",
		Candidates: []modelgateway.CandidateDoc{{
			Provider: "upstream-under-test", Model: model,
			Modality: modelgateway.ModalityText, InferenceClass: modelgateway.InferenceClassCloud, Priority: 0,
		}},
	}}}
}

func firstChoice(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("the response carries no choices: %v", body)
	}
	choice, _ := choices[0].(map[string]any)
	return choice
}
