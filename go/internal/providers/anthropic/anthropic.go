// Package anthropic is the anthropic adapter behind the Provider interface (docs/adr/0004). Real,
// not a stub: a genuine HTTP client for the Messages API (POST /v1/messages), translating this
// gateway's common OpenAI-Chat-Completions-shaped input into Anthropic's request shape and its
// response back into providers.NormalizedChatResponse — the one output shape every adapter returns,
// so a caller never needs to know which provider actually served a call.
package anthropic

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
const Name = "anthropic"

// Provider is an alias for the shared interface (go/internal/providers) every model adapter
// implements — kept here so existing references to anthropic.Provider keep working.
type Provider = providers.Provider

const defaultMaxTokens = 1024

// Adapter implements Provider against the real Anthropic Messages API.
type Adapter struct {
	APIKey     string
	BaseURL    string // defaults to https://api.anthropic.com if empty
	Version    string // defaults to "2023-06-01" if empty — the anthropic-version header
	HTTPClient *http.Client
}

func (a *Adapter) baseURL() string {
	if a.BaseURL != "" {
		return a.BaseURL
	}
	return "https://api.anthropic.com"
}

func (a *Adapter) version() string {
	if a.Version != "" {
		return a.Version
	}
	return "2023-06-01"
}

func (a *Adapter) httpClient() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return http.DefaultClient
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
	System    string             `json:"system,omitempty"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		// Pointers, not ints: an absent field means this response carried no cache accounting at
		// all, which is a different fact from "nothing was cached" and must stay that way as far as
		// the FinOps ledger (MDL-012).
		CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

type anthropicError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Decide translates renderedContext (OpenAI-Chat-Completions-shaped: model, messages[]) into an
// Anthropic Messages API request, calls it, and returns providers.NormalizedChatResponse.
// Anthropic requires max_tokens and puts the system prompt in its own top-level field rather than
// as a "system"-role message — both translations happen here, not in the caller.
func (a *Adapter) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	model, _ := renderedContext["model"].(string)
	rawMessages, _ := renderedContext["messages"].([]any)

	maxTokens := defaultMaxTokens
	if v, ok := renderedContext["max_tokens"].(int); ok && v > 0 {
		maxTokens = v
	}

	var system string
	messages := make([]anthropicMessage, 0, len(rawMessages))
	for _, raw := range rawMessages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if role == "system" {
			system = content
			continue
		}
		messages = append(messages, anthropicMessage{Role: role, Content: content})
	}

	reqBody := anthropicRequest{Model: model, MaxTokens: maxTokens, Messages: messages, System: system}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encoding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.baseURL(), "/")+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("anthropic: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.APIKey)
	httpReq.Header.Set("anthropic-version", a.version())

	resp, err := a.httpClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr anthropicError
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Error.Message != "" {
			return nil, fmt.Errorf("anthropic: status %d: %s: %s", resp.StatusCode, apiErr.Error.Type, apiErr.Error.Message)
		}
		return nil, fmt.Errorf("anthropic: status %d: %s", resp.StatusCode, string(body))
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("anthropic: decoding response: %w", err)
	}

	var text strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}

	return providers.NormalizedChatResponseWithUsage(parsed.Model, text.String(), parsed.StopReason, anthropicUsage(parsed)), nil
}

// CachingCapability: Anthropic supports explicit cache-control breakpoints, not automatic prefix
// caching — the caller must mark what to cache (ADR-003's Budgeter decides where).
func (a *Adapter) CachingCapability() string { return "explicit_breakpoints" }

// CostModel: billed per token, both input and output.
func (a *Adapter) CostModel() string { return "token_based" }

// anthropicUsage maps Anthropic's token accounting onto the shape agreed with the Synaptum and
// Axonium teams, where the input counter is the TOTAL input and cache_read is the subset of it that
// came from cache.
//
// This is the one adapter where that mapping is arithmetic rather than a copy. Anthropic reports
// input_tokens EXCLUDING cached reads, so the inclusive convention requires adding them; every
// OpenAI-shaped provider already reports the total and the adapter copies it. The convention was
// argued on the premise that copying cannot be done wrong — true, but it does not cover this
// provider, and this is exactly where the arithmetic gets forgotten.
//
// Falsifying this is cheap and worth doing if anything looks off: send one request with a cached
// prefix and compare input_tokens + cache_read_input_tokens against the total the provider bills.
// If Anthropic ever reports input_tokens inclusively, this addition becomes a double count.
//
// cache_creation_input_tokens is reported but deliberately NOT added: by the same agreement,
// cache_write is not part of input. Note the consequence, which MDL-012 does not close — cache
// writes are billed at a premium and the ModelPolicyBundle has no cache rates, so those tokens are
// now visible in the ledger without being priced. Visible and unpriced beats invisible.
func anthropicUsage(parsed anthropicResponse) providers.Usage {
	u := providers.Usage{
		PromptTokens:     parsed.Usage.InputTokens,
		CompletionTokens: parsed.Usage.OutputTokens,
		CacheReadTokens:  parsed.Usage.CacheReadInputTokens,
		CacheWriteTokens: parsed.Usage.CacheCreationInputTokens,
	}
	if parsed.Usage.CacheReadInputTokens != nil {
		u.PromptTokens += *parsed.Usage.CacheReadInputTokens
	}
	return u
}
