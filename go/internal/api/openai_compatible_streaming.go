package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
	w http.ResponseWriter, r *http.Request, candidates []modelgateway.Candidate, body map[string]any,
	dataSensitivity, runID, agentRef string, governance map[string]any,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "aeon_streaming_error", "this server cannot stream responses")
		return
	}

	// VRT-AEON-003 A-3: an SSE body has nowhere to carry the `aeon` object the non-streamed path
	// returns, so the same facts go in headers — and they must be set BEFORE the first chunk, since
	// that is when WriteHeader happens and headers stop being writable. The ceiling itself was
	// already checked by the caller, before this function and before any provider call.
	for key, value := range governanceHeaders(governance) {
		w.Header().Set(key, value)
	}

	ctx := r.Context()
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	headersSent := false
	// The usage a streamed call reports arrives in a CHUNK, not in a return value — StreamResult
	// carries no Output — so the last one seen is kept for the ledger write below.
	var streamedUsage *providers.Usage
	var streamedModel string

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

		// A pure lifecycle marker (FND-004's stream_start/text_end/...) carries nothing an
		// OpenAI-compatible client expects, so it is not put on the wire. The events are Aeon's internal
		// vocabulary and this endpoint's whole purpose is being unsurprising to clients that know only
		// OpenAI's shape — emitting empty `data:` frames at them would be extending someone else's format.
		if chunk.Usage != nil {
			streamedUsage = chunk.Usage
		}
		if chunk.Model != "" {
			streamedModel = chunk.Model
		}
		if !chunk.CarriesWireContent() {
			return nil
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

	h.recordStreamedCost(r, result, streamedUsage, streamedModel, runID, agentRef)

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
	// Kept in its own key rather than folded into content (MDL-016): a client rendering a live
	// answer must be able to show deliberation differently, or not at all, and concatenating the two
	// makes that choice for it — irreversibly, since nothing downstream can tell them apart again.
	if chunk.ReasoningDelta != "" {
		delta["reasoning_content"] = chunk.ReasoningDelta
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
		// A counter the provider did not report is OMITTED here too, not emitted as zero (MDL-014).
		//
		// This is an external surface, so the trade is real and worth stating: a client that assumes the
		// keys are present now reads undefined instead of a number, and in Python that is falsy and may
		// well be treated as 0 — the same fabricated zero, reconstructed on the far side. Omitting is
		// still the right half of that trade, because emitting 0 is a POSITIVE CLAIM we cannot support,
		// while an absent key is at worst ambiguous. OpenAI's own schema allows a null usage on a chunk,
		// so a client that handles the spec handles this.
		usage := map[string]any{}
		if chunk.Usage.PromptTokens != nil {
			usage["prompt_tokens"] = *chunk.Usage.PromptTokens
		}
		if chunk.Usage.CompletionTokens != nil {
			usage["completion_tokens"] = *chunk.Usage.CompletionTokens
		}
		if chunk.Usage.PromptTokens != nil && chunk.Usage.CompletionTokens != nil {
			usage["total_tokens"] = *chunk.Usage.PromptTokens + *chunk.Usage.CompletionTokens
		}
		envelope["usage"] = usage
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

// recordStreamedCost writes this streamed call's cost to the ledger, through the SAME recordCost
// /decide and the non-streamed path use.
//
// WHAT IT CANNOT CARRY, stated rather than left to be discovered: a streamed response has no
// provider_request_id, no served_by_instance and no idempotent_replay_of, because those arrive in
// response HEADERS of a single-shot call and a stream has none. So the entry is the usage and the
// price, and the fields that cannot be known are absent rather than invented.
//
// A stream that reported NO usage is still recorded, with the counters nil. That is OBS-008's rule
// applied here: a call that leaves no row cannot be audited, and an audit cannot tell it from a call
// that never happened. It also makes the ceiling work for streams — model_calls counts even when
// tokens do not.
func (h *OpenAICompatibleHandlers) recordStreamedCost(
	r *http.Request, result *modelgateway.StreamResult, usage *providers.Usage, servedModel, runID, agentRef string,
) {
	if h.Governance == nil || result == nil {
		return
	}
	output := map[string]any{}
	if usage != nil {
		block := map[string]any{}
		// Each counter copied only when the provider reported it (MDL-014): a nil written as 0 here
		// would become a measured zero in the ledger, which is the one thing this column must not say.
		if usage.PromptTokens != nil {
			block["prompt_tokens"] = *usage.PromptTokens
		}
		if usage.CompletionTokens != nil {
			block["completion_tokens"] = *usage.CompletionTokens
		}
		if usage.ReasoningTokens != nil {
			block["reasoning_tokens"] = *usage.ReasoningTokens
		}
		output["usage"] = block
	}
	if servedModel != "" {
		output["model"] = servedModel
	}
	// recordCost reads the decision through this shape, so a StreamResult is adapted to it rather
	// than the ledger-write logic being duplicated for streams.
	decision := &modelgateway.DecisionResult{
		ProviderUsed: result.ProviderUsed,
		Model:        result.Model,
		Output:       output,
		Attempts:     result.Attempts,
	}
	// The response map is discarded: the bytes are already on the wire, so there is nowhere left to
	// report cost for a stream. The LEDGER is the point here, and the headers above already told the
	// caller whether this call was governed at all.
	h.Governance.recordCost(r, decision, runID, agentRef, map[string]any{})
}

// governanceHeaders projects the governance report onto the SSE surface's only available carrier.
func governanceHeaders(governance map[string]any) map[string]string {
	headers := map[string]string{}
	if governance == nil {
		return headers
	}
	if governed, ok := governance["governed"].(bool); ok {
		headers["X-Aeon-Governed"] = strconv.FormatBool(governed)
	}
	if reasons, ok := governance["ungoverned_reasons"].([]string); ok && len(reasons) > 0 {
		// Joined with "; " and not newline-separated: a header value with a newline in it is a
		// response-splitting bug, and net/http would reject it outright.
		headers["X-Aeon-Ungoverned-Reason"] = strings.Join(reasons, "; ")
	}
	if runID, ok := governance["run_id"].(string); ok && runID != "" {
		headers[RunIDHeader] = runID
	}
	return headers
}
