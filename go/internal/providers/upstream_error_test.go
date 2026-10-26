package providers_test

import (
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/providers"
)

// SEC-007: a provider's non-2xx must not carry the provider's free text.
//
// THE BODIES BELOW ARE MEASURED, not invented — four content-provoked failures against the real
// Prometheus gateway on 2026-10-25, each with the sentinel `Ana-Quispe-12345678` in the request. That
// matters for the same reason the normalization corpus prefers recordings: a test written against a
// body we imagined would assert that our code handles an error we wrote.
//
// What the measurement said, and it is more precise than "it leaks": the message CONTENT did not come
// back in any of the four, and REQUEST VALUES did — `got 'wizard'` and, verbatim, the model name. So
// the cut is by FIELD and not by length: every echoed value arrived inside `detail`/`message`, and
// everything actionable arrived beside it.
func TestAnUpstreamErrorCarriesNoProviderText(t *testing.T) {
	const sentinel = "Ana-Quispe-12345678"

	cases := []struct {
		name   string
		status int
		body   string
		// mustSay is what an operator acts on and must survive the cut.
		mustSay []string
	}{{
		name:   "problem details con un valor de la petición dentro de detail",
		status: 400,
		body: `{"type":"https://prometheus.internal/errors/unknown-model","title":"Unknown Model",` +
			`"status":400,"detail":"Model 'no-existe-` + sentinel + `' is not registered. Use GET /v1/models ` +
			`for the list of available models.","instance":"/v1/chat/completions",` +
			`"request_id":"c150a117-5b22-4bfb-a677-6004be82cca5"}`,
		mustSay: []string{"400", "unknown-model", "Unknown Model", "c150a117-5b22-4bfb-a677-6004be82cca5"},
	}, {
		name:   "validation error: el campo se conserva, el valor no",
		status: 422,
		body: `{"type":"https://prometheus.internal/errors/validation-error","title":"Validation Error",` +
			`"status":422,"detail":"body.messages.0.role: Value error, role must be one of {'system', 'user',` +
			` 'tool', 'assistant'}, got '` + sentinel + `'","request_id":"d7"}`,
		mustSay: []string{"422", "validation-error", "d7"},
	}, {
		name:    "sobre de error de OpenAI",
		status:  400,
		body:    `{"error":{"code":400,"message":"Assistant message must contain either 'content' or 'tool_calls'!","type":"invalid_request_error"}}`,
		mustSay: []string{"400", "invalid_request_error"},
	}, {
		name:    "un cuerpo que no es JSON, como la página de error de un proxy",
		status:  502,
		body:    `<html><body>502 Bad Gateway: ` + sentinel + `</body></html>`,
		mustSay: []string{"502", "not JSON"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providers.UpstreamError("prov", tc.status, []byte(tc.body)).Error()
			if strings.Contains(got, sentinel) {
				t.Errorf("the error repeats a value from the request: %q — a redactor downstream cannot "+
					"fix this, so the content must never enter the message", got)
			}
			// And no free text at all: the two fields where every echoed value arrived.
			for _, leaked := range []string{"is not registered", "Value error", "must contain either"} {
				if strings.Contains(got, leaked) {
					t.Errorf("the error carries the provider's free text (%q): %q", leaked, got)
				}
			}
			for _, want := range tc.mustSay {
				if !strings.Contains(got, want) {
					t.Errorf("the error does not say %q: %q — cutting blindly would remove the very thing "+
						"that diagnoses, which is what Argus warned about", want, got)
				}
			}
			// The length is what is left of the body itself, and it is worth keeping: it distinguishes an
			// empty answer from a long one without saying anything about either.
			if !strings.Contains(got, "bytes") {
				t.Errorf("the error does not report the body length: %q", got)
			}
		})
	}
}

// TestTheRequestIDIsTheWayBackToTheDetail is the property that makes the cut honest rather than lossy.
//
// The free text is not destroyed, it is one lookup away in the provider's own logs — by an id the
// provider gave us. Without this, "we removed the message" would be a loss and Argus's objection to
// cutting blindly would stand.
func TestTheRequestIDIsTheWayBackToTheDetail(t *testing.T) {
	body := `{"type":"https://x/errors/validation-error","status":422,"detail":"secreto","request_id":"req-42"}`
	got := providers.UpstreamError("prov", 422, []byte(body)).Error()
	if !strings.Contains(got, "request_id=req-42") {
		t.Fatalf("no request_id in %q: without it the detail is unreachable and the cut IS a loss", got)
	}
}
