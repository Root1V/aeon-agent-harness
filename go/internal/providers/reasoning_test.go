package providers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/providers"
	openaicompatible "github.com/aeon-ai/aeon/go/internal/providers/openai_compatible"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
)

// reasoningBody is a transcription of a real response from the live Prometheus deployment
// (gpt-oss-20b-mxfp4, 2026-09-13). The shape matters more than the words: content is empty, the
// whole allowance went into reasoning_content, and finish_reason is "length". That is not an edge
// case for this model — it is what a reasoning model does whenever the budget is tight.
const reasoningBody = `{
  "model": "gpt-oss-20b-mxfp4",
  "choices": [{
    "index": 0,
    "finish_reason": "length",
    "message": {
      "role": "assistant",
      "content": "",
      "reasoning_content": "We need to respond only with the word \"funciona\". The user says: Responde solo con la palabra."
    }
  }],
  "usage": {"prompt_tokens": 75, "completion_tokens": 20, "total_tokens": 95,
            "completion_tokens_details": {"reasoning_tokens": 20}}
}`

// fakeUpstream serves the chat response and, on the token path, a real-shaped OAuth2 answer — so
// the prometheus_inference adapter exercises its whole path (mint a token, then call) rather than a
// shortcut built for the test.
func fakeUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"test-token","token_type":"Bearer","expires_in":600}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestReasoningContentSurvivesNormalization is MDL-016's acceptance test.
//
// Both adapters used to read `content` alone. Against the live deployment with max_tokens 20 that
// produced an empty answer, finish_reason "length", twenty billed output tokens, and no explanation
// anywhere of why there was no text — the reason had travelled in a field being discarded.
//
// The two adapters parse this independently, so the test drives both against one body. Nothing
// forced them to agree before, and a discrepancy here would mean the same response normalizes two
// ways depending on which provider served it.
func TestReasoningContentSurvivesNormalization(t *testing.T) {
	ctx := context.Background()
	srv := fakeUpstream(t, reasoningBody)

	adapters := map[string]providers.Provider{
		"openai_compatible":    &openaicompatible.Adapter{BaseURL: srv.URL},
		"prometheus_inference": prometheusinference.New(srv.URL, srv.URL, "id", "secret", "inference:read", "gpt-oss-20b-mxfp4"),
	}

	for name, adapter := range adapters {
		t.Run(name+" preserves reasoning, its token count, and does not merge it into content", func(t *testing.T) {
			out, err := adapter.Decide(ctx, map[string]any{"model": "m", "messages": []any{}})
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}

			choices, _ := out["choices"].([]any)
			if len(choices) == 0 {
				t.Fatal("no choices")
			}
			choice, _ := choices[0].(map[string]any)
			message, _ := choice["message"].(map[string]any)

			reasoning, _ := message["reasoning_content"].(string)
			if reasoning == "" {
				t.Fatal("reasoning_content was dropped — the caller sees an empty answer it paid for, with no reason given")
			}
			if !strings.Contains(reasoning, "funciona") {
				t.Errorf("reasoning_content = %q, want the model's actual deliberation", reasoning)
			}

			content, _ := message["content"].(string)
			if content != "" {
				t.Errorf("content = %q, want it left empty — merging reasoning into the answer cannot be undone downstream", content)
			}

			usage, _ := out["usage"].(map[string]any)
			if got := usage["reasoning_tokens"]; got != 20 {
				t.Errorf("usage.reasoning_tokens = %v, want 20 — the expensive part of this call is the part that was invisible", got)
			}
			if got := choice["finish_reason"]; got != "length" {
				t.Errorf("finish_reason = %v, want length", got)
			}
		})
	}

	t.Run("a provider that reports no reasoning says nothing rather than zero", func(t *testing.T) {
		// Three states again: absent means "this provider does not break reasoning out", and a zero
		// would claim it measured and found none. The FinOps ledger would record the difference.
		plain := fakeUpstream(t, `{"model":"m","choices":[{"index":0,"finish_reason":"stop",
			"message":{"role":"assistant","content":"hola"}}],
			"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
		adapter := &openaicompatible.Adapter{BaseURL: plain.URL}

		out, err := adapter.Decide(ctx, map[string]any{"model": "m", "messages": []any{}})
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		choices, _ := out["choices"].([]any)
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if _, present := message["reasoning_content"]; present {
			t.Error("reasoning_content is present for a provider that sent none")
		}
		usage, _ := out["usage"].(map[string]any)
		if _, present := usage["reasoning_tokens"]; present {
			t.Error("reasoning_tokens is present for a provider that did not report it")
		}
	})

	t.Run("in streaming, reasoning arrives in its own flow and before the answer", func(t *testing.T) {
		// The order is the reason this cannot be a single field: deliberation streams first, so a
		// client watching only `content` sees nothing for the whole thinking phase — which looks
		// exactly like a stalled stream.
		sse := "data: " + mustJSON(map[string]any{
			"model":   "m",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "pensando… "}}},
		}) + "\n\ndata: " + mustJSON(map[string]any{
			"model":   "m",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hola"}}},
		}) + "\n\ndata: [DONE]\n\n"

		streamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, sse)
		}))
		t.Cleanup(streamSrv.Close)

		adapter := &openaicompatible.Adapter{BaseURL: streamSrv.URL}
		var answer, thinking strings.Builder
		var order []string
		err := adapter.DecideStream(ctx, map[string]any{"model": "m", "messages": []any{}}, func(c providers.Chunk) error {
			if c.ReasoningDelta != "" {
				thinking.WriteString(c.ReasoningDelta)
				order = append(order, "reasoning")
			}
			if c.Delta != "" {
				answer.WriteString(c.Delta)
				order = append(order, "content")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("DecideStream: %v", err)
		}

		if thinking.String() != "pensando… " {
			t.Errorf("reasoning stream = %q, want the deliberation", thinking.String())
		}
		if answer.String() != "hola" {
			t.Errorf("answer stream = %q, want only the answer — the two flows were merged", answer.String())
		}
		if len(order) != 2 || order[0] != "reasoning" || order[1] != "content" {
			t.Errorf("flow order = %v, want reasoning before content", order)
		}
	})
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}
