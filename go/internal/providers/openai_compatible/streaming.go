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
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
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
			return nil
		}

		var parsed streamChunk
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			return fmt.Errorf("openai_compatible: decoding stream chunk: %w", err)
		}

		chunk := providers.Chunk{Model: parsed.Model}
		if len(parsed.Choices) > 0 {
			chunk.Delta = parsed.Choices[0].Delta.Content
			chunk.FinishReason = parsed.Choices[0].FinishReason
		}
		if parsed.Usage != nil {
			chunk.Usage = &providers.Usage{
				PromptTokens:     parsed.Usage.PromptTokens,
				CompletionTokens: parsed.Usage.CompletionTokens,
			}
		}

		if err := yield(chunk); err != nil {
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("openai_compatible: reading stream: %w", err)
	}
	return nil
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
