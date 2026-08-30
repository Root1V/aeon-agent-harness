// Package openai is the openai adapter behind the Provider interface (docs/adr/0004). Real, not a
// stub: a genuine HTTP client for the Chat Completions API (POST /v1/chat/completions). OpenAI's
// own wire shape is what this gateway's common input/output convention was modeled after, so
// translation here is mostly pass-through plus bearer auth — but the response still goes through
// providers.NormalizedChatResponse explicitly, the same as every other adapter, rather than being
// returned as-is: a caller must never need to know it happens to be talking to OpenAI.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aeon-ai/aeon/go/internal/providers"
)

// Name identifies this adapter in ModelPolicyBundle candidates (proto/schemas/model_profile.schema.json).
const Name = "openai"

// Provider is an alias for the shared interface (go/internal/providers) every model adapter
// implements — kept here so existing references to openai.Provider keep working.
type Provider = providers.Provider

// Adapter implements Provider against the real OpenAI Chat Completions API. Also works, unchanged,
// against Azure OpenAI or any other deployment that speaks this exact API by pointing BaseURL at
// it — see MDL-007 (openai_compatible) for the generic vLLM/Ollama/TGI case that needs no API key.
type Adapter struct {
	APIKey     string
	BaseURL    string // defaults to https://api.openai.com if empty
	HTTPClient *http.Client
}

func (a *Adapter) baseURL() string {
	if a.BaseURL != "" {
		return a.BaseURL
	}
	return "https://api.openai.com"
}

func (a *Adapter) httpClient() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return http.DefaultClient
}

type openAIChoice struct {
	Index        int    `json:"index"`
	FinishReason string `json:"finish_reason"`
	Message      struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type openAIError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Decide sends renderedContext (already OpenAI-Chat-Completions-shaped: model, messages[], and
// optionally max_tokens/temperature/tools/tool_choice) essentially as-is, adds bearer auth, and
// normalizes the response.
func (a *Adapter) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(renderedContext)
	if err != nil {
		return nil, fmt.Errorf("openai: encoding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, strings.TrimRight(a.baseURL(), "/")+"/v1/chat/completions", bytes.NewReader(raw),
	)
	if err != nil {
		return nil, fmt.Errorf("openai: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.APIKey)

	resp, err := a.httpClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr openAIError
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Error.Message != "" {
			return nil, fmt.Errorf("openai: status %d: %s: %s", resp.StatusCode, apiErr.Error.Type, apiErr.Error.Message)
		}
		return nil, fmt.Errorf("openai: status %d: %s", resp.StatusCode, string(body))
	}

	var parsed openAIResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("openai: decoding response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("openai: response had no choices")
	}

	choice := parsed.Choices[0]
	return providers.NormalizedChatResponse(
		parsed.Model, choice.Message.Content, choice.FinishReason, parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens,
	), nil
}

// CachingCapability: OpenAI caches automatically by prompt prefix — no explicit breakpoints needed.
func (a *Adapter) CachingCapability() string { return "automatic_prefix" }

// CostModel: billed per token, both input (prompt) and output (completion).
func (a *Adapter) CostModel() string { return "token_based" }
