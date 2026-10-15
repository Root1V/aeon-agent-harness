package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/providers"
)

// nonStreamingProvider implements Provider and NOT providers.StreamingProvider, which is the only
// way to reach the fallback the header exists to announce: the gateway calls Decide once and
// delivers the whole answer as a single chunk.
type nonStreamingProvider struct{}

func (nonStreamingProvider) Decide(_ context.Context, rendered map[string]any) (map[string]any, error) {
	model, _ := rendered["model"].(string)
	return providers.NormalizedChatResponseFrom(providers.ChatResult{
		Model: model, Content: "one shot", FinishReason: "stop",
	}), nil
}
func (nonStreamingProvider) CachingCapability() string { return "none" }
func (nonStreamingProvider) CostModel() string         { return "token_based" }

// TestStreamingHeadersReachTheClient is OBS-018.
//
// `X-Aeon-Streamed: false` was set AFTER the stream loop, and by then a chunk had been written —
// which is when WriteHeader happens and headers stop being writable, a rule this very file states
// a few lines above where it was broken. Go ignores a later Header().Set in silence. So in the ONE
// case the header exists for, it never arrived, and the comment promising a caller "can tell from
// the header rather than having to infer it from chunk sizes" was false.
//
// NOTHING TESTED IT, which is why nothing noticed: measured, no test in this repo asserted this
// header before this one.
func TestStreamingHeadersReachTheClient(t *testing.T) {
	gw := modelgateway.New()
	gw.RegisterProvider("one-shot", nonStreamingProvider{})
	bundle := modelgateway.ModelPolicyBundleDoc{
		Profiles: []modelgateway.ModelProfileDoc{{
			Profile: "streaming-test",
			Candidates: []modelgateway.CandidateDoc{{
				Provider: "one-shot", Model: "fake-model", Modality: modelgateway.ModalityText,
				InferenceClass: modelgateway.InferenceClassCloud, Priority: 0,
			}},
		}},
	}
	mux := http.NewServeMux()
	(&OpenAICompatibleHandlers{Gateway: gw, Bundle: bundle}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)

	resp := postStream(t, context.Background(), srv.URL, true)
	defer resp.Body.Close()

	// READ THE BODY FIRST. The headers a client sees are the ones sent with the response, so draining
	// the stream before asserting is what makes this test about what ARRIVED rather than about what
	// the handler set afterwards on a map nobody reads.
	var chunks int
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(strings.TrimSpace(scanner.Text()), "data: ") {
			chunks++
		}
	}
	if chunks == 0 {
		t.Fatal("no chunks arrived, so this measures nothing")
	}

	if got := resp.Header.Get("X-Aeon-Streamed"); got != "false" {
		t.Errorf("X-Aeon-Streamed = %q, want \"false\": this provider cannot stream, the answer came as one "+
			"chunk, and a caller that has to infer that from chunk sizes is what the header exists to avoid", got)
	}
}

// TestTheUnsendableCountReachesTheStreamingClient is the other half OBS-018 unlocked. VRT-SYN-004's
// count was reportable on the two non-streaming doors (a body field, and the `aeon` object) and on
// the span, and NOT here — the comment in the handler said so and explained why. The reason was real
// and the conclusion was not: the gateway knows the count before it calls the provider, it simply
// had no way to say so before the first chunk.
func TestTheUnsendableCountReachesTheStreamingClient(t *testing.T) {
	gw := modelgateway.New()
	gw.RegisterProvider("one-shot", nonStreamingProvider{})
	bundle := modelgateway.ModelPolicyBundleDoc{
		Profiles: []modelgateway.ModelProfileDoc{{
			Profile: "streaming-test",
			Candidates: []modelgateway.CandidateDoc{{
				Provider: "one-shot", Model: "fake-model", Modality: modelgateway.ModalityText,
				InferenceClass: modelgateway.InferenceClassCloud, Priority: 0,
			}},
		}},
	}
	mux := http.NewServeMux()
	(&OpenAICompatibleHandlers{Gateway: gw, Bundle: bundle}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)

	// An assistant turn with nothing the wire can carry: the gateway drops it (MDL-022) and must say
	// that it did, because dropping a message narrows the request the caller built.
	resp := postStreamBody(t, srv.URL, map[string]any{
		"model":  "streaming-test",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "hola"},
			map[string]any{"role": "assistant", "reasoning_content": "pensando"},
			map[string]any{"role": "user", "content": "sigue"},
		},
	})
	defer resp.Body.Close()
	drain(t, resp.Body)

	if got := resp.Header.Get("X-Aeon-Unsendable-Turns-Dropped"); got != "1" {
		t.Errorf("X-Aeon-Unsendable-Turns-Dropped = %q, want \"1\": a caller on this door had no way to "+
			"learn its request had been narrowed", got)
	}

	t.Run("and is ABSENT when nothing was dropped", func(t *testing.T) {
		resp := postStreamBody(t, srv.URL, map[string]any{
			"model": "streaming-test", "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hola"}},
		})
		defer resp.Body.Close()
		drain(t, resp.Body)
		// Absent and not "0": absence is what tells a caller its request went out as it built it.
		if got := resp.Header.Get("X-Aeon-Unsendable-Turns-Dropped"); got != "" {
			t.Errorf("the header is present on an untouched request: %q", got)
		}
	})
}

func drain(t *testing.T, body io.Reader) {
	t.Helper()
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
	}
}

// postStreamBody posts an arbitrary body to the streaming surface with this package's credential.
func postStreamBody(t *testing.T, baseURL string, body map[string]any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(authorize(req))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}
