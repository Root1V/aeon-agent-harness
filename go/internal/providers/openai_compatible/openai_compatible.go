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
		// MDL-016: several OpenAI-compatible servers (llama.cpp among them) return the chain of
		// thought in its own field. Reading only `content` reports an empty answer for a response
		// that was generated and billed.
		ReasoningContent string `json:"reasoning_content"`
		// FND-004: the calls the model wants made. `arguments` is a JSON STRING on the wire and the
		// contract says the adapter parses it — the loop should not have to guess what it received.
		ToolCalls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
}

// openAICompatibleUsage is the wire's usage object.
//
// A NAMED type rather than an anonymous struct inside the response, so a test double can build one. The
// anonymous version meant the doubles had to restate the whole shape, and when FND-004 added
// prompt_tokens_details one of them silently served a body without it.
type openAICompatibleUsage struct {
	// Pointers (MDL-014): nil means the key was absent — including when the whole `usage` object is,
	// since then nothing sets them. That also covers the partial case, a usage object missing one
	// counter, which a pointer on the object alone would not.
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	// OpenAI's shape for the reasoning breakdown, which some servers mirror. A pointer so that
	// "not reported" stays distinct from "reasoned for zero tokens" all the way to the ledger.
	CompletionTokensDetails struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// FND-004: how many of the input tokens were served from cache. `input` is INCLUSIVE of these,
	// per the shared contract, so this is read and copied rather than subtracted — copying cannot be
	// done wrong, and forgetting a subtraction would double-count the cached half with no error.
	PromptTokensDetails struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type openAICompatibleResponse struct {
	ID      string                   `json:"id"`
	Model   string                   `json:"model"`
	Choices []openAICompatibleChoice `json:"choices"`
	// A POINTER, so nil answers "there was no usage object at all" — the question that decides whether
	// counters are reported or derived (FND-004's `estimated`).
	//
	// The first attempt added a SECOND field with the same `json:"usage"` tag to answer presence. Go
	// ignores BOTH fields when two share a tag at the same level, so every counter decoded as nil and the
	// adapter reported every call as unmeasured. It compiled, and the failure pointed the comfortable way:
	// "not measured" looks prudent. Caught by decoding a real body and printing the result.
	Usage *openAICompatibleUsage `json:"usage"`
	// Timings is llama.cpp's own accounting, which Prometheus surfaces. Present on responses that ALSO
	// carry usage, which is why the contract has to say which wins.
	Timings *struct {
		CacheN     *int `json:"cache_n"`
		PromptN    *int `json:"prompt_n"`
		PredictedN *int `json:"predicted_n"`
	} `json:"timings"`
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

	toolCalls := make([]providers.ToolCall, 0, len(choice.Message.ToolCalls))
	for _, tc := range choice.Message.ToolCalls {
		args, err := providers.DecodeToolArguments(tc.Function.Arguments)
		if err != nil {
			// Surfaced, not swallowed. A provider that sent unparseable arguments produced a call nobody can
			// execute correctly, and the contract says so explicitly: a silent empty object would run the
			// tool with no arguments and report success.
			return nil, fmt.Errorf("openai_compatible: tool call %q: %w", tc.Function.Name, err)
		}
		toolCalls = append(toolCalls, providers.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: args})
	}

	return providers.NormalizedChatResponseFrom(providers.ChatResult{
		Model:            parsed.Model,
		Content:          choice.Message.Content,
		ReasoningContent: choice.Message.ReasoningContent,
		ToolCalls:        toolCalls,
		FinishReason:     NormalizeFinishReason(choice.FinishReason),
		Usage:            parsed.usage(),
	}), nil
}

// usage applies the contract's rule about which accounting wins.
//
// IF THERE IS A `usage` OBJECT, IT WINS, and `timings` is ignored even when present — which it usually is,
// since Prometheus surfaces both. Reported is never replaced by derived. Only when there is no usage
// object at all do the counters come from `timings`, and then every one of them is marked estimated.
//
// The two branches set `estimated` to false and true respectively rather than leaving it nil, because in
// both cases we KNOW the provenance. Nil is reserved for an adapter that cannot say.
func (r openAICompatibleResponse) usage() providers.Usage {
	if r.Usage != nil {
		return providers.ReportedUsage(providers.Usage{
			PromptTokens:     r.Usage.PromptTokens,
			CompletionTokens: r.Usage.CompletionTokens,
			ReasoningTokens:  r.Usage.CompletionTokensDetails.ReasoningTokens,
			CacheReadTokens:  r.Usage.PromptTokensDetails.CachedTokens,
		})
	}
	if r.Timings == nil {
		// No usage and no timings. Everything stays nil — unmeasured, not zero — and `estimated` is FALSE
		// rather than nil or true: nothing was derived, so claiming a derivation would be as wrong as
		// claiming a measurement. This is the case the corpus covers with a hand-written body, because
		// after the v8 re-record no real response lacks usage any more.
		return providers.ReportedUsage(providers.Usage{})
	}
	// Derived from timings (llama.cpp emits no usage chunk, not even when asked). `input` is
	// prompt_n + cache_n because the contract's input is INCLUSIVE of cache, and cache_n alone is the
	// cached subset.
	u := providers.Usage{}
	if r.Timings.PromptN != nil || r.Timings.CacheN != nil {
		total := 0
		if r.Timings.PromptN != nil {
			total += *r.Timings.PromptN
		}
		if r.Timings.CacheN != nil {
			total += *r.Timings.CacheN
		}
		u.PromptTokens = &total
	}
	u.CacheReadTokens = r.Timings.CacheN
	u.CompletionTokens = r.Timings.PredictedN
	// reasoning and cache_write stay nil: they do not exist in this source, and deriving them from
	// something that does not measure them is how a fabricated number enters the ledger.
	return providers.DerivedUsage(u)
}

// NormalizeFinishReason applies the shared contract's finish-reason table
// (evals/contracts/normalizacion/spec.md): the four known reasons pass through, and an absent or
// unknown one becomes "stop".
//
// Mapping an unknown value to "stop" is lossy and is the contract's call, not ours — several
// OpenAI-compatible servers simply omit the field on a normal completion, and a consumer that has
// to special-case an empty string learns nothing the mapping does not already tell it. Found by
// running Synaptum's corpus: an empty stream produced no reason at all.
func NormalizeFinishReason(reason string) string {
	switch reason {
	case "stop", "length", "tool_calls", "content_filter":
		return reason
	default:
		return "stop"
	}
}

// CachingCapability: most self-hosted serving stacks have no prompt caching — ADR-003's Budgeter
// falls back to aggressive offload for this provider, same as prometheus_inference.
func (a *Adapter) CachingCapability() string { return "none" }

// CostModel: self-hosted inference is billed by compute (GPU-seconds), not per token.
func (a *Adapter) CostModel() string { return "compute_based" }

// ServerAddress is VRT-AXO-002's `server.address` (providers.ServerAddresser).
//
// This adapter is the reason the attribute matters most: every self-hosted server looks the same in
// a trace otherwise — same provider name, same model id, different machine.
func (a *Adapter) ServerAddress() string { return providers.HostOf(a.BaseURL) }
