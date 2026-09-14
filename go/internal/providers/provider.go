// Package providers defines the single Provider interface every model adapter implements
// (docs/adr/0004). The Model Gateway (aeon-modelgw) depends only on this interface, never on a
// concrete provider SDK. Each adapter (anthropic, openai, gemini, prometheus_inference,
// openai_compatible) lives in its own subpackage and implements this interface — Go's structural
// typing means an adapter doesn't need to import this package to satisfy it, but they all do
// anyway so there is exactly one definition to keep in sync, not five.
package providers

import "context"

// Provider is implemented by every model adapter.
type Provider interface {
	// Decide sends rendered context to the underlying model and returns raw provider output for
	// the gateway to turn into a typed Decision (proto/schemas/decision.schema.json).
	Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error)
	// CachingCapability reports what prompt/context caching this provider supports, so the
	// Context Budgeter (ADR-003) can choose a strategy accordingly.
	CachingCapability() string // "automatic_prefix" | "explicit_breakpoints" | "none"
	// CostModel reports how this provider bills, so FinOps (OBS-003) can compare "cost per
	// successful task" across token-based and compute-based providers.
	CostModel() string // "token_based" | "compute_based"
}

// NormalizedChatResponse is the ONE output shape every adapter's Decide() must return, regardless
// of the wire format its own provider actually speaks (Anthropic's content blocks, Gemini's
// candidates, OpenAI's choices). This is what makes cross-provider conformance checking
// mechanical: a caller (or a future provider_conformance suite) reads choices[0].message.content
// and usage.* the same way no matter which adapter served the call. Each adapter's own tests
// verify it translates its provider's real response into exactly this shape.
func NormalizedChatResponse(model, content, finishReason string, promptTokens, completionTokens int) map[string]any {
	return map[string]any{
		"model": model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
}

// ChatResult is everything an adapter extracted from one complete response. It exists because the
// alternative was a sixth positional argument, and because reasoning is not a variant of content:
// keeping them as separate fields is what stops an adapter from concatenating them "just this once".
type ChatResult struct {
	Model        string
	Content      string
	FinishReason string
	// ReasoningContent is the model's chain of thought, which several providers return alongside
	// the answer and in its own field (MDL-016). Discarding it is not harmless: with a tight token
	// budget a reasoning model spends the whole allowance thinking, `content` comes back empty, and
	// the caller sees an unexplained blank answer that was nonetheless billed. Verified against the
	// live deployment on 2026-09-13 — max_tokens 20 produced exactly that.
	ReasoningContent string
	Usage            Usage
}

// NormalizedChatResponseFrom builds the normalized response from a complete ChatResult.
//
// reasoning_content is emitted only when the provider sent some, so its absence keeps meaning "this
// provider does not report reasoning" rather than "it thought about nothing".
func NormalizedChatResponseFrom(r ChatResult) map[string]any {
	response := NormalizedChatResponseWithUsage(r.Model, r.Content, r.FinishReason, r.Usage)
	if r.ReasoningContent == "" {
		return response
	}
	choices, _ := response["choices"].([]any)
	if len(choices) == 0 {
		return response
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	message["reasoning_content"] = r.ReasoningContent
	return response
}

// NormalizedChatResponseWithUsage is NormalizedChatResponse for an adapter whose provider reports
// cache accounting (MDL-012). The cache counters appear in the usage block only when the provider
// actually reported them: an absent key means "not reported", which is a different fact from a zero
// and has to stay distinguishable all the way to the ledger.
func NormalizedChatResponseWithUsage(model, content, finishReason string, u Usage) map[string]any {
	response := NormalizedChatResponse(model, content, finishReason, u.PromptTokens, u.CompletionTokens)
	usage, _ := response["usage"].(map[string]any)
	if u.CacheReadTokens != nil {
		usage["cache_read_tokens"] = *u.CacheReadTokens
	}
	if u.CacheWriteTokens != nil {
		usage["cache_write_tokens"] = *u.CacheWriteTokens
	}
	if u.ReasoningTokens != nil {
		usage["reasoning_tokens"] = *u.ReasoningTokens
	}
	return response
}
