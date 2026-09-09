package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	openaicompatible "github.com/aeon-ai/aeon/go/internal/providers/openai_compatible"
)

// fakeStreamingUpstream is a real HTTP server speaking the OpenAI Chat Completions SSE format, and
// — the part that matters for INT-008 — it records how far it got before the reader went away. That
// counter is the only honest way to test cancellation: a client that merely stops reading proves
// nothing, since the generation could have completed upstream and been discarded locally. Here the
// upstream itself reports whether it was cut short.
type fakeStreamingUpstream struct {
	totalChunks   int
	chunkDelay    time.Duration
	mu            sync.Mutex
	chunksWorked  int
	sawDisconnect bool
}

func (f *fakeStreamingUpstream) written() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chunksWorked, f.sawDisconnect
}

func (f *fakeStreamingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if stream, _ := body["stream"].(bool); !stream {
		http.Error(w, "this fake only serves streaming requests", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	for i := 0; i < f.totalChunks; i++ {
		select {
		case <-r.Context().Done():
			// The reader disconnected: a real model server stops generating here. Recording it is
			// what lets the test assert the abort actually crossed the wire.
			f.mu.Lock()
			f.sawDisconnect = true
			f.mu.Unlock()
			return
		case <-time.After(f.chunkDelay):
		}

		chunk := map[string]any{
			"model":   "fake-stream-model",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": fmt.Sprintf("tok%d ", i)}}},
		}
		encoded, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
			f.mu.Lock()
			f.sawDisconnect = true
			f.mu.Unlock()
			return
		}
		if flusher != nil {
			flusher.Flush()
		}

		f.mu.Lock()
		f.chunksWorked++
		f.mu.Unlock()
	}

	final := map[string]any{
		"model":   "fake-stream-model",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
	}
	encoded, _ := json.Marshal(final)
	fmt.Fprintf(w, "data: %s\n\n", encoded)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// newStreamingTestServer wires the real chain the feature is about: a real openai_compatible
// adapter pointed at the fake upstream, a real Gateway routing to it, and the real
// OpenAICompatibleHandlers serving SSE.
func newStreamingTestServer(t *testing.T, upstream *fakeStreamingUpstream) *httptest.Server {
	t.Helper()
	upstreamSrv := httptest.NewServer(upstream)
	t.Cleanup(upstreamSrv.Close)

	gw := modelgateway.New()
	gw.RegisterProvider(openaicompatible.Name, &openaicompatible.Adapter{BaseURL: upstreamSrv.URL})

	bundle := modelgateway.ModelPolicyBundleDoc{
		Profiles: []modelgateway.ModelProfileDoc{{
			Profile: "streaming-test",
			Candidates: []modelgateway.CandidateDoc{
				{Provider: openaicompatible.Name, Model: "fake-stream-model", Priority: 0},
			},
		}},
	}

	mux := http.NewServeMux()
	(&OpenAICompatibleHandlers{Gateway: gw, Bundle: bundle}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postStream(t *testing.T, ctx context.Context, url string, stream bool) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    "streaming-test",
		"messages": []any{map[string]any{"role": "user", "content": "hola"}},
		"stream":   stream,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	return resp
}

// TestModelGatewayStreamsAndCancelsMidStream is INT-008's acceptance test, and the concrete answer
// to the tripartite agreement's P5 for the hop Aeon owns: does a cancellation actually reach the
// upstream model, or does it only stop the local reader?
func TestModelGatewayStreamsAndCancelsMidStream(t *testing.T) {
	t.Run("a full stream delivers every delta in order and terminates with [DONE]", func(t *testing.T) {
		upstream := &fakeStreamingUpstream{totalChunks: 4, chunkDelay: time.Millisecond}
		srv := newStreamingTestServer(t, upstream)

		resp := postStream(t, context.Background(), srv.URL, true)
		defer resp.Body.Close()

		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("Content-Type = %q, want text/event-stream", ct)
		}

		var deltas []string
		sawDone := false
		var usage map[string]any
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				sawDone = true
				break
			}
			var parsed map[string]any
			if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
				t.Fatalf("chunk is not valid JSON: %v (%q)", err, payload)
			}
			if parsed["object"] != "chat.completion.chunk" {
				t.Fatalf("object = %v, want chat.completion.chunk", parsed["object"])
			}
			if u, ok := parsed["usage"].(map[string]any); ok {
				usage = u
			}
			choices, _ := parsed["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			choice, _ := choices[0].(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if content, ok := delta["content"].(string); ok && content != "" {
				deltas = append(deltas, content)
			}
		}

		if !sawDone {
			t.Error("stream never terminated with [DONE]")
		}
		got := strings.Join(deltas, "")
		if got != "tok0 tok1 tok2 tok3 " {
			t.Errorf("assembled deltas = %q, want every chunk in order", got)
		}
		if usage == nil {
			t.Error("expected the final chunk to carry usage — FinOps would be blind for streamed calls otherwise")
		} else if usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(7) {
			t.Errorf("usage = %v, want the upstream's real counts", usage)
		}
	})

	t.Run("cancelling mid-stream really stops generation upstream", func(t *testing.T) {
		// Enough chunks, slow enough, that the upstream is unambiguously still generating when the
		// client walks away.
		upstream := &fakeStreamingUpstream{totalChunks: 50, chunkDelay: 20 * time.Millisecond}
		srv := newStreamingTestServer(t, upstream)

		ctx, cancel := context.WithCancel(context.Background())
		resp := postStream(t, ctx, srv.URL, true)

		// Read a couple of real chunks, then abandon the request the way a client with an
		// exhausted budget would.
		scanner := bufio.NewScanner(resp.Body)
		read := 0
		for scanner.Scan() && read < 2 {
			if strings.HasPrefix(strings.TrimSpace(scanner.Text()), "data: ") {
				read++
			}
		}
		if read < 2 {
			t.Fatal("did not receive any streamed chunk before cancelling")
		}
		cancel()
		resp.Body.Close()

		// Give the abort time to travel: client → gateway → adapter → upstream.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, disconnected := upstream.written(); disconnected {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		written, disconnected := upstream.written()
		if !disconnected {
			t.Fatal("the upstream never observed the disconnect — cancellation did not reach the model, which is exactly what INT-008 exists to prevent")
		}
		if written >= upstream.totalChunks {
			t.Fatalf("upstream wrote all %d chunks: generation was not cut short, only discarded locally", written)
		}
		t.Logf("upstream stopped after %d of %d chunks", written, upstream.totalChunks)
	})

	t.Run("a non-streaming request is unaffected", func(t *testing.T) {
		upstream := &fakeStreamingUpstream{totalChunks: 2, chunkDelay: time.Millisecond}
		srv := newStreamingTestServer(t, upstream)

		resp := postStream(t, context.Background(), srv.URL, false)
		defer resp.Body.Close()

		// The fake only serves streaming, so a non-streamed call routes down the old path and fails
		// there — proving the branch is taken on "stream", not applied to everything.
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("a request without stream:true must not get an SSE response, got %q", ct)
		}
	})
}
