package providers

import (
	"encoding/json"
	"fmt"
	"strings"
)

// UpstreamError renders a provider's non-2xx WITHOUT the provider's free text (SEC-007).
//
// WHY, and it is Veritium's point from VRT-AEON-008 one layer over: four adapters did
// `fmt.Errorf("...: status %d: %s", status, string(body))`. That body is text the provider wrote, it
// can repeat the request, and through the failed-candidate path it reaches `span.RecordError` — so
// Argus's store and the caller's 502 — with nothing in between able to clean it. Their sentence is the
// one that settles it: a name cannot be caught by a regular expression, so a redactor downstream
// cannot fix this and the content must never enter the message.
//
// MEASURED FIRST, which was the order Argus asked for and the reason this is not a blind truncation.
// Four content-provoked failures against the real Prometheus gateway, each carrying a sentinel in the
// request:
//
//	rol inválido                         422   the sentinel did NOT come back; `detail` echoed `got 'wizard'`
//	assistant sin content ni tool_calls  400   nothing of the request came back
//	temperature fuera de rango           422   nothing of the request came back
//	modelo inexistente                   400   `detail` echoed the model name VERBATIM
//
// So for this provider the message content did not come back, and REQUEST VALUES DID. The capability
// Veritium named is real and the realistic leak is a field value rather than a whole message — which
// is exactly why the cut is by FIELD and not by length: every value the provider echoed came back
// inside `detail`/`message`, and everything actionable came back beside it.
//
// WHAT IS KEPT is the structured part, which is enum-like and carries nothing of the caller: the
// problem-details `type`, `title` and `status`, OpenAI's `error.type`/`error.code`/`error.param`, the
// body's length — and `request_id`, which is the part that makes this honest. The free text is not
// lost, it is one lookup away in the provider's own logs, by an id the provider itself gave us. That
// answers Argus's warning that cutting blindly can remove the very thing that diagnoses.
func UpstreamError(provider string, status int, body []byte) error {
	return fmt.Errorf("%s: status %d (%s)", provider, status, describeUpstreamBody(body))
}

// upstreamEnvelope covers the two shapes these providers actually send: RFC 9457 problem details
// (Prometheus, measured) and OpenAI's error object (OpenAI, Anthropic and Gemini all nest an object
// with a machine-readable type or code). `detail`/`message` are deliberately NOT fields here: a struct
// that cannot hold the free text cannot leak it by someone adding it to a format string later.
type upstreamEnvelope struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	RequestID string `json:"request_id"`
	TraceID   string `json:"trace_id"`
	Error     struct {
		Type  string `json:"type"`
		Code  any    `json:"code"`
		Param string `json:"param"`
	} `json:"error"`
}

func describeUpstreamBody(body []byte) string {
	parts := []string{fmt.Sprintf("%d bytes", len(body))}
	var env upstreamEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		// Not JSON at all: an HTML error page from a proxy, or a truncated response. Its length is all
		// that can be said about it without quoting it.
		return strings.Join(append(parts, "body is not JSON"), ", ")
	}
	add := func(label, value string) {
		if value != "" {
			parts = append(parts, label+"="+value)
		}
	}
	// `type` LAST SEGMENT ONLY for problem details, which is what identifies the error class; the full
	// URI is the provider's namespace repeated on every line.
	add("type", lastSegment(env.Type))
	add("title", env.Title)
	add("error.type", env.Error.Type)
	if env.Error.Code != nil {
		add("error.code", fmt.Sprintf("%v", env.Error.Code))
	}
	// `param` is a FIELD PATH (`body.messages.0.role`), not a value. It is the single most useful thing
	// in a validation failure and it names where, never what.
	add("error.param", env.Error.Param)
	add("request_id", env.RequestID)
	add("trace_id", env.TraceID)
	if len(parts) == 1 {
		parts = append(parts, "no machine-readable fields in the body")
	}
	return strings.Join(parts, ", ")
}

func lastSegment(uri string) string {
	trimmed := strings.TrimRight(uri, "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}
