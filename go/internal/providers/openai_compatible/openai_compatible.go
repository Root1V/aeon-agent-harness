// Package openaicompatible is the openai_compatible adapter behind the Provider interface
// (docs/adr/0004) — the generic fallback for vLLM/Ollama/TGI/LM Studio and anything else that
// speaks the OpenAI Chat Completions wire format without being OpenAI itself. Real, not a stub:
// the same real HTTP client as go/internal/providers/openai, minus the assumptions that don't
// hold for self-hosted servers — no default BaseURL (there is no "the" self-hosted endpoint) and
// no required API key (most local servers accept none; some want an arbitrary bearer token, which
// this adapter sends only if one is configured).
package openaicompatible

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
const Name = "openai_compatible"

// Provider is an alias for the shared interface (go/internal/providers) every model adapter
// implements — kept here so existing references to openaicompatible.Provider keep working.
type Provider = providers.Provider

// Adapter implements Provider against any server speaking the OpenAI Chat Completions wire
// format. BaseURL is required — unlike go/internal/providers/openai, there is no sensible default
// for a self-hosted deployment (see deploy/compose/docker-compose.yml's "local-llm" profile,
// which runs vLLM at a project-local address).
type Adapter struct {
	BaseURL    string
	APIKey     string // optional — many local servers (vLLM/Ollama default config) need none
	HTTPClient *http.Client
}

func (a *Adapter) httpClient() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return http.DefaultClient
}

type openAICompatibleChoice struct {
	Index        int    `json:"index"`
	FinishReason string `json:"finish_reason"`
	Message      struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}

type openAICompatibleResponse struct {
	ID      string                    `json:"id"`
	Model   string                    `json:"model"`
	Choices []openAICompatibleChoice  `json:"choices"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Decide sends renderedContext (OpenAI-Chat-Completions-shaped) to BaseURL + /v1/chat/completions
// — the same request shape as go/internal/providers/openai, since that's the whole point of this
// adapter existing.
func (a *Adapter) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	if a.BaseURL == "" {
		return nil, fmt.Errorf("openai_compatible: BaseURL is required (no default for a self-hosted server)")
	}

	raw, err := json.Marshal(renderedContext)
	if err != nil {
		return nil, fmt.Errorf("openai_compatible: encoding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+"/v1/chat/completions", bytes.NewReader(raw),
	)
	if err != nil {
		return nil, fmt.Errorf("openai_compatible: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if a.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.APIKey)
	}

	resp, err := a.httpClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai_compatible: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai_compatible: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai_compatible: status %d: %s", resp.StatusCode, string(body))
	}

	var parsed openAICompatibleResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("openai_compatible: decoding response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("openai_compatible: response had no choices")
	}

	choice := parsed.Choices[0]
	return providers.NormalizedChatResponse(
		parsed.Model, choice.Message.Content, choice.FinishReason, parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens,
	), nil
}

// CachingCapability: most self-hosted serving stacks have no prompt caching — ADR-003's Budgeter
// falls back to aggressive offload for this provider, same as prometheus_inference.
func (a *Adapter) CachingCapability() string { return "none" }

// CostModel: self-hosted inference is billed by compute (GPU-seconds), not per token.
func (a *Adapter) CostModel() string { return "compute_based" }
