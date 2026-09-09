package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/providers"
)

// streamChatCompletions serves POST /v1/chat/completions with "stream": true as real SSE (INT-008).
//
// The cancellation path, which is the point of the feature: when the client disconnects, net/http
// cancels r.Context(); the yield below sees it and returns an error; the gateway stops the adapter;
// the adapter closes its upstream connection; the real model stops generating. Nothing waits for a
// complete response. Same path serves a budget cut — whoever wants to stop only has to make yield
// fail.
//
// Errors have two regimes, and the split is forced by SSE rather than chosen: before the first
// chunk nothing has been written, so a failure is a normal JSON error with a real status code;
// after it, the 200 and the headers are already on the wire, so a failure can only be reported as a
// terminal SSE error event. A client that ignores that event sees a truncated stream, which is the
// same thing every OpenAI-compatible server does for the same reason.
func (h *OpenAICompatibleHandlers) streamChatCompletions(
	w http.ResponseWriter, r *http.Request, candidates []modelgateway.Candidate, body map[string]any, dataSensitivity string,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "aeon_streaming_error", "this server cannot stream responses")
		return
	}

	ctx := r.Context()
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	headersSent := false

	yield := func(chunk providers.Chunk) error {
		// Checked before writing, not after: once the client is gone there is no point producing
		// another token upstream, and returning here is what unwinds the whole chain.
		if err := ctx.Err(); err != nil {
			return err
		}

		if !headersSent {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			headersSent = true
		}

		if err := writeSSEData(w, streamChunkEnvelope(id, created, chunk)); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	result, err := h.Gateway.DecideStream(ctx, candidates, body, dataSensitivity, yield)
	if err != nil {
		if !headersSent {
			writeOpenAIError(w, http.StatusBadGateway, "aeon_routing_error", err.Error())
			return
		}
		// A client that went away is not an error to report — there is nobody left to read it.
		if ctx.Err() == nil {
			_ = writeSSEData(w, map[string]any{"error": map[string]any{"message": err.Error(), "type": "aeon_streaming_error"}})
			flusher.Flush()
		}
		return
	}

	// A provider without streaming support was served as one chunk; the caller can tell from the
	// header rather than having to infer it from chunk sizes.
	if result != nil && !result.Streamed {
		w.Header().Set("X-Aeon-Streamed", "false")
	}

	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err == nil {
		flusher.Flush()
	}
}

// streamChunkEnvelope wraps a normalized chunk in the OpenAI streaming shape
// (object: "chat.completion.chunk", choices[].delta) so any existing OpenAI-compatible client
// reads it without special-casing Aeon — the same reasoning as toOpenAIChatCompletionResponse for
// the non-streamed path.
func streamChunkEnvelope(id string, created int64, chunk providers.Chunk) map[string]any {
	delta := map[string]any{}
	if chunk.Delta != "" {
		delta["content"] = chunk.Delta
	}

	choice := map[string]any{"index": 0, "delta": delta}
	if chunk.FinishReason != "" {
		choice["finish_reason"] = chunk.FinishReason
	}

	envelope := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   chunk.Model,
		"choices": []any{choice},
	}
	if chunk.Usage != nil {
		envelope["usage"] = map[string]any{
			"prompt_tokens":     chunk.Usage.PromptTokens,
			"completion_tokens": chunk.Usage.CompletionTokens,
			"total_tokens":      chunk.Usage.PromptTokens + chunk.Usage.CompletionTokens,
		}
	}
	return envelope
}

func writeSSEData(w http.ResponseWriter, payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding stream chunk: %w", err)
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", encoded)
	return err
}
