package openaicompatible

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/aeon-ai/aeon/go/internal/providers"
)

// sseDataPrefix and sseDone are the two literals the OpenAI Chat Completions streaming format is
// built from: every event is a line "data: {json}", and the stream ends with "data: [DONE]".
const (
	sseDataPrefix = "data: "
	sseDone       = "[DONE]"
)

type streamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content string `json:"content"`
			// MDL-016: the chain of thought streams in its own field and arrives BEFORE any answer
			// token. A reader that only watches `content` sees nothing at all while a reasoning
			// model works, which looks identical to a stalled stream.
			ReasoningContent string `json:"reasoning_content"`
			// FND-004: tool calls stream as fragments correlated ONLY by index. The name arrives in the
			// first fragment and the arguments are split across the rest, so matching on anything but the
			// index would lose every fragment after the first.
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		// Pointers inside an already-pointer Usage (MDL-014). Two levels, two different facts: the outer
		// nil means this chunk carried no usage at all, the inner nil means the usage object carried the
		// counter's key but not its value — which a streaming upstream really does, since it emits usage
		// only on the final chunk and only when asked.
		PromptTokens            *int `json:"prompt_tokens"`
		CompletionTokens        *int `json:"completion_tokens"`
		CompletionTokensDetails struct {
			ReasoningTokens *int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		PromptTokensDetails struct {
			CachedTokens *int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	// Timings is llama.cpp's accounting, which streams on the final chunk of a Prometheus response and is
	// the ONLY source of counters there — it emits no usage chunk at all, not even when asked.
	Timings *struct {
		CacheN     *int `json:"cache_n"`
		PromptN    *int `json:"prompt_n"`
		PredictedN *int `json:"predicted_n"`
	} `json:"timings"`
}

// streamPhases tracks which lifecycle phase is open so the adapter can emit the contract's
// start/delta/end cycle from a wire format that has no such thing.
//
// The wire sends deltas and nothing else, so every start and end here is INFERRED from a transition. The
// one that matters is closing reasoning when a tool call opens rather than when the stream ends: the
// contract names it, and a consumer watching only deltas cannot see it.
type streamPhases struct {
	openText      bool
	openReasoning bool
	openToolCall  int // index of the tool call currently open, -1 when none
	// toolArgs accumulates argument fragments per index. The contract is explicit that these are NOT
	// valid JSON until the end, so nothing parses them here.
	toolArgs  map[int]*strings.Builder
	toolNames map[int]string
	toolIDs   map[int]string
	order     []int
}

func newStreamPhases() *streamPhases {
	return &streamPhases{
		openToolCall: -1,
		toolArgs:     map[int]*strings.Builder{},
		toolNames:    map[int]string{},
		toolIDs:      map[int]string{},
	}
}

// closeText and closeReasoning emit the end of a phase if one is open.
func (ph *streamPhases) closeText(yield func(providers.Chunk) error) error {
	if !ph.openText {
		return nil
	}
	ph.openText = false
	return yield(providers.Chunk{Event: providers.EventTextEnd})
}

func (ph *streamPhases) closeReasoning(yield func(providers.Chunk) error) error {
	if !ph.openReasoning {
		return nil
	}
	ph.openReasoning = false
	return yield(providers.Chunk{Event: providers.EventReasoningEnd})
}

// closeToolCall parses the accumulated arguments ONCE and emits the end event carrying the finished call.
//
// Once, here, is the whole point: the fragments are not valid JSON until now, so an adapter parsing each
// delta would produce errors on the happy path. A fragment sequence that never becomes valid JSON is an
// error rather than an empty object, the same rule the non-streaming path follows.
func (ph *streamPhases) closeToolCall(yield func(providers.Chunk) error) error {
	if ph.openToolCall < 0 {
		return nil
	}
	idx := ph.openToolCall
	ph.openToolCall = -1

	raw := ""
	if b, ok := ph.toolArgs[idx]; ok {
		raw = b.String()
	}
	args, err := providers.DecodeToolArguments(raw)
	if err != nil {
		return fmt.Errorf("openai_compatible: tool call %q (stream index %d): %w", ph.toolNames[idx], idx, err)
	}
	return yield(providers.Chunk{
		Event:         providers.EventToolCallEnd,
		ToolCallIndex: idx,
		ToolCall:      &providers.ToolCall{ID: ph.toolIDs[idx], Name: ph.toolNames[idx], Arguments: args},
	})
}

// closeAll ends every open phase, in the order the contract's finish event expects.
func (ph *streamPhases) closeAll(yield func(providers.Chunk) error) error {
	if err := ph.closeReasoning(yield); err != nil {
		return err
	}
	if err := ph.closeText(yield); err != nil {
		return err
	}
	return ph.closeToolCall(yield)
}

// DecideStream implements providers.StreamingProvider (INT-008). Cancellation is real and reaches
// the upstream server two ways, both of which close the connection so the model stops generating:
// ctx being cancelled (the client of the gateway went away), or yield returning an error (a budget
// ran out mid-stream). Neither waits for the response to finish.
func (a *Adapter) DecideStream(ctx context.Context, renderedContext map[string]any, yield func(providers.Chunk) error) error {
	if a.BaseURL == "" {
		return fmt.Errorf("openai_compatible: BaseURL is required (no default for a self-hosted server)")
	}

	raw, err := json.Marshal(streamingRequestBody(renderedContext))
	if err != nil {
		return fmt.Errorf("openai_compatible: encoding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+"/v1/chat/completions", bytes.NewReader(raw),
	)
	if err != nil {
		return fmt.Errorf("openai_compatible: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if a.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.APIKey)
	}

	resp, err := a.httpClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("openai_compatible: request failed: %w", err)
	}
	// Closing the body is what actually aborts an in-flight generation upstream — it must run on
	// every exit path, including the early return when yield says stop.
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openai_compatible: status %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	// A single SSE event can carry a large delta; the default 64KB token limit is generous for
	// deltas but not for a provider that batches, so raise the ceiling rather than fail mid-stream.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// stream_start before anything is read, so a consumer has an opening event even for a stream that
	// turns out to be empty — which the contract says is valid, not an error.
	if err := yield(providers.Chunk{Event: providers.EventStreamStart}); err != nil {
		return err
	}
	phases := newStreamPhases()
	var finishReason string
	var finalUsage *providers.Usage
	sawFinishReason := false
	for scanner.Scan() {
		// Check cancellation between events too, not only inside yield: a provider that stops
		// sending without closing would otherwise leave this loop blocked on Scan.
		if err := ctx.Err(); err != nil {
			return err
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, sseDataPrefix) {
			continue
		}
		payload := strings.TrimPrefix(line, sseDataPrefix)
		if payload == sseDone {
			// [DONE] ends the stream, but it is not itself a finish reason. Falling through to the
			// terminal-chunk logic below is what makes an empty stream — valid per the shared
			// contract — report a normal stop instead of nothing at all.
			//
			// And it stops at the FIRST one: anything after it is not read. The contract requires exactly
			// one sentinel per response, so a second would be the upstream misbehaving, and continuing to
			// parse past it is how a reader ends up accepting whatever follows.
			break
		}

		var parsed streamChunk
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			return fmt.Errorf("openai_compatible: decoding stream chunk: %w", err)
		}

		if len(parsed.Choices) > 0 {
			delta := parsed.Choices[0].Delta

			// Reasoning first, because it arrives first. Opening its phase closes nothing: nothing can be
			// open before it.
			if delta.ReasoningContent != "" {
				if !phases.openReasoning {
					phases.openReasoning = true
					if err := yield(providers.Chunk{Event: providers.EventReasoningStart, Model: parsed.Model}); err != nil {
						return err
					}
				}
				if err := yield(providers.Chunk{
					Event: providers.EventReasoningDelta, ReasoningDelta: delta.ReasoningContent, Model: parsed.Model,
				}); err != nil {
					return err
				}
			}

			// Any answer token CLOSES the reasoning phase. The transition is what the end event means, and
			// inferring it here is the only place it can be inferred — the wire never says it.
			if delta.Content != "" {
				if err := phases.closeReasoning(yield); err != nil {
					return err
				}
				if !phases.openText {
					phases.openText = true
					if err := yield(providers.Chunk{Event: providers.EventTextStart, Model: parsed.Model}); err != nil {
						return err
					}
				}
				if err := yield(providers.Chunk{
					Event: providers.EventTextDelta, Delta: delta.Content, Model: parsed.Model,
				}); err != nil {
					return err
				}
			}

			for _, tc := range delta.ToolCalls {
				// A tool call opening closes the reasoning phase too — the case the contract names
				// explicitly, because a consumer watching deltas alone cannot see it and would leave the
				// deliberation on screen for the rest of the turn.
				if err := phases.closeReasoning(yield); err != nil {
					return err
				}
				if phases.openToolCall != tc.Index {
					// A different index means the previous call is complete. Closing it here rather than at
					// the end of the stream is what lets several calls stream interleaved.
					if err := phases.closeToolCall(yield); err != nil {
						return err
					}
					phases.openToolCall = tc.Index
					if _, seen := phases.toolArgs[tc.Index]; !seen {
						phases.toolArgs[tc.Index] = &strings.Builder{}
						phases.order = append(phases.order, tc.Index)
					}
					if err := yield(providers.Chunk{
						Event: providers.EventToolCallStart, ToolCallIndex: tc.Index, Model: parsed.Model,
					}); err != nil {
						return err
					}
				}
				// Name and id arrive in the first fragment only, so they are recorded rather than expected
				// again; overwriting with a later empty string would erase them.
				if tc.Function.Name != "" {
					phases.toolNames[tc.Index] = tc.Function.Name
				}
				if tc.ID != "" {
					phases.toolIDs[tc.Index] = tc.ID
				}
				if tc.Function.Arguments != "" {
					phases.toolArgs[tc.Index].WriteString(tc.Function.Arguments)
					if err := yield(providers.Chunk{
						Event: providers.EventToolCallDelta, ToolCallIndex: tc.Index,
						ToolCallDelta: tc.Function.Arguments, Model: parsed.Model,
					}); err != nil {
						return err
					}
				}
			}

			if raw := parsed.Choices[0].FinishReason; raw != "" {
				finishReason = NormalizeFinishReason(raw)
				sawFinishReason = true
			}
		}
		// Usage is held for the finish event rather than forwarded as it arrives. The contract requires
		// usage ON `finish`, so a consumer that read the deltas never has to look for it elsewhere.
		if u := streamUsage(parsed); u != nil {
			finalUsage = u
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("openai_compatible: reading stream: %w", err)
	}

	// Every open phase closes before finish, so `finish` is genuinely last.
	if err := phases.closeAll(yield); err != nil {
		return err
	}

	// A stream that ends without ever stating a reason still ended, and the shared contract says an
	// absent reason is a normal stop. Emitting it keeps the rule in one place: otherwise every consumer
	// decides separately what an empty reason means, and an empty stream (valid, per the contract) would
	// look indistinguishable from a truncated one.
	if !sawFinishReason {
		finishReason = NormalizeFinishReason("")
	}
	if finalUsage == nil {
		// No usage and no timings anywhere in the stream. Every counter stays unmeasured and `estimated` is
		// FALSE rather than nil or true: nothing was derived, so claiming a derivation would be as wrong as
		// claiming a measurement.
		finalUsage = &providers.Usage{}
		f := false
		finalUsage.Estimated = &f
	}
	return yield(providers.Chunk{Event: providers.EventFinish, FinishReason: finishReason, Usage: finalUsage})
}

// streamUsage applies the contract's precedence to a streamed chunk: a reported usage object wins, and
// `timings` is only consulted when there is none.
func streamUsage(parsed streamChunk) *providers.Usage {
	if parsed.Usage != nil {
		u := providers.ReportedUsage(providers.Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			ReasoningTokens:  parsed.Usage.CompletionTokensDetails.ReasoningTokens,
			CacheReadTokens:  parsed.Usage.PromptTokensDetails.CachedTokens,
		})
		return &u
	}
	if parsed.Timings == nil {
		return nil
	}
	u := providers.Usage{}
	if parsed.Timings.PromptN != nil || parsed.Timings.CacheN != nil {
		total := 0
		if parsed.Timings.PromptN != nil {
			total += *parsed.Timings.PromptN
		}
		if parsed.Timings.CacheN != nil {
			total += *parsed.Timings.CacheN
		}
		u.PromptTokens = &total
	}
	u.CacheReadTokens = parsed.Timings.CacheN
	u.CompletionTokens = parsed.Timings.PredictedN
	derived := providers.DerivedUsage(u)
	return &derived
}

// streamingRequestBody copies renderedContext and turns it into a streaming request, without
// mutating the caller's map.
//
// stream_options.include_usage is injected only when the caller hasn't set stream_options itself:
// it is the standard way to get token counts back on the final chunk (vLLM and OpenAI both honor
// it), and without it a streamed call reports no usage at all, which would leave the FinOps ledger
// blind for exactly the calls most likely to be long and expensive. A caller that knows its server
// rejects the field can pass its own stream_options to override.
func streamingRequestBody(renderedContext map[string]any) map[string]any {
	body := make(map[string]any, len(renderedContext)+2)
	for k, v := range renderedContext {
		body[k] = v
	}
	body["stream"] = true
	if _, ok := body["stream_options"]; !ok {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return body
}
