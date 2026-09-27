// Package providers defines the single Provider interface every model adapter implements
// (docs/adr/0004). The Model Gateway (aeon-modelgw) depends only on this interface, never on a
// concrete provider SDK. Each adapter (anthropic, openai, gemini, prometheus_inference,
// openai_compatible) lives in its own subpackage and implements this interface — Go's structural
// typing means an adapter doesn't need to import this package to satisfy it, but they all do
// anyway so there is exactly one definition to keep in sync, not five.
package providers

import (
	"context"
	"encoding/json"
)

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
func NormalizedChatResponse(model, content, finishReason string, promptTokens, completionTokens *int) map[string]any {
	return map[string]any{
		"model": model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": finishReason,
			},
		},
		"usage": usageBlock(promptTokens, completionTokens),
	}
}

// usageBlock renders the two base counters, OMITTING each one the provider did not report (MDL-014).
//
// Omission and not zero, for the reason the cache counters already worked this way: a key that is
// absent says "not measured", and a zero says "measured, and it was none". The ledger and the FinOps
// dashboard need those apart, and `total_tokens` is omitted too when either part is missing — a total
// computed from one known half and one absent one would be a smaller number wearing the shape of a
// complete one.
func usageBlock(promptTokens, completionTokens *int) map[string]any {
	usage := map[string]any{}
	if promptTokens != nil {
		usage["prompt_tokens"] = *promptTokens
	}
	if completionTokens != nil {
		usage["completion_tokens"] = *completionTokens
	}
	if promptTokens != nil && completionTokens != nil {
		usage["total_tokens"] = *promptTokens + *completionTokens
	}
	return usage
}

// Tokens returns a pointer to n, for adapters whose provider DID report a counter.
//
// A named helper rather than a local variable at every call site: `providers.Tokens(0)` reads as "the
// provider said zero" and a bare nil reads as "it said nothing", which is exactly the distinction
// MDL-014 exists to keep.
func Tokens(n int) *int { return &n }

// ChatResult is everything an adapter extracted from one complete response. It exists because the
// alternative was a sixth positional argument, and because reasoning is not a variant of content:
// keeping them as separate fields is what stops an adapter from concatenating them "just this once".
type ChatResult struct {
	// ProviderRequestID is the provider's own identifier for this call, when it issues one, used to
	// reconcile our cost figure against theirs (OBS-007). Empty for providers that issue none.
	ProviderRequestID string
	// IdempotentReplayOf names the generation that was actually billed, when this response is a
	// replay of an identical earlier call (OBS-006). Empty when this call really generated. The usage
	// block on a replay repeats the ORIGINAL call's tokens, so billing it again inflates the ledger.
	IdempotentReplayOf string
	// ServedByInstance is the deployment that actually answered, when the provider reports one.
	//
	// Carried here by MDL-009 because the Axonium SDK surfaces it and dropping it would waste a fact
	// nothing else can recover. The ledger imputes cost by the bundle's model, which is correct — you
	// pay for the profile, not the deployment — but "which deployment answered" is where an incident
	// starts, and today it is recorded nowhere. Recording it and reconciling the discrepancy is
	// OBS-005; this field is what makes that possible.
	ServedByInstance string

	Model        string
	Content      string
	FinishReason string
	// ReasoningContent is the model's chain of thought, which several providers return alongside
	// the answer and in its own field (MDL-016). Discarding it is not harmless: with a tight token
	// budget a reasoning model spends the whole allowance thinking, `content` comes back empty, and
	// the caller sees an unexplained blank answer that was nonetheless billed. Verified against the
	// live deployment on 2026-09-13 — max_tokens 20 produced exactly that.
	ReasoningContent string
	// ReasoningSignature is the opaque blob some providers require returned intact on the next turn.
	// Transported, never interpreted.
	ReasoningSignature string
	// ToolCalls are the calls the model asked for, with ARGUMENTS ALREADY DECODED (FND-004). Decoded at
	// the adapter because that is where the provider's wire format is known; a string reaching the loop
	// would make every consumer parse it, and one of them would tolerate a failure.
	ToolCalls []ToolCall
	Usage     Usage
}

// NormalizedChatResponseFrom builds the normalized response from a complete ChatResult.
//
// reasoning_content is emitted only when the provider sent some, so its absence keeps meaning "this
// provider does not report reasoning" rather than "it thought about nothing".
func NormalizedChatResponseFrom(r ChatResult) map[string]any {
	response := NormalizedChatResponseWithUsage(r.Model, r.Content, r.FinishReason, r.Usage)
	// OBS-007: the provider's own id for this call, when it gives one. It is the join key between our
	// cost ledger and the platform's, and it lives nowhere else — the platform returns it in a header,
	// so if it is not carried here it is lost by the time anything could reconcile. Absent for
	// providers that report no such id, which is why the key is omitted rather than set to "".
	if r.ProviderRequestID != "" {
		response["provider_request_id"] = r.ProviderRequestID
	}
	// OBS-006: present ONLY on a replay, so its absence means "this call really generated" rather
	// than "unknown" — the same reason the cache counters are omitted instead of zeroed.
	if r.IdempotentReplayOf != "" {
		response["idempotent_replay_of"] = r.IdempotentReplayOf
	}
	if r.ServedByInstance != "" {
		response["served_by_instance"] = r.ServedByInstance
	}
	choices, _ := response["choices"].([]any)
	if len(choices) == 0 {
		return response
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if r.ReasoningContent != "" {
		message["reasoning_content"] = r.ReasoningContent
	}

	// FND-004: the typed parts of the contract, ALONGSIDE `content` rather than replacing it.
	//
	// Both are needed and they answer different questions. `content` is the OpenAI-compatible string that
	// INT-002's endpoint must keep emitting, because every client of that surface expects it. `content_parts`
	// is the contract's unified view — thinking separated from text, tool calls carried as parts with decoded
	// arguments. Replacing `content` would break the compatibility the endpoint exists for; omitting the
	// parts is the gap this closes.
	if parts := ContentParts(r.ReasoningContent, r.Content, r.ToolCalls, r.ReasoningSignature); len(parts) > 0 {
		message["content_parts"] = parts
	}
	// tool_calls in OpenAI's own shape too, so an OpenAI-compatible client of INT-002 sees them where it
	// looks. The arguments go back out as a STRING there, because that is what that wire format says — the
	// decoded form lives in content_parts.
	if len(r.ToolCalls) > 0 {
		message["tool_calls"] = openAIShapedToolCalls(r.ToolCalls)
		if _, ok := message["content"]; ok && r.Content == "" {
			// A tool-call-only response has null content on the wire, not "". The distinction is the
			// contract's: an empty text and the absence of text are not the same message.
			message["content"] = nil
		}
	}
	return response
}

// openAIShapedToolCalls re-encodes decoded arguments for the OpenAI-compatible surface.
//
// Re-encoding rather than keeping the original string: the original may never have existed (a provider
// with a different wire shape), so carrying it would make this field available only for some providers.
// The decoded map is the single source, and this is a projection of it.
func openAIShapedToolCalls(calls []ToolCall) []any {
	out := make([]any, 0, len(calls))
	for i, tc := range calls {
		raw, err := json.Marshal(tc.Arguments)
		if err != nil {
			// Cannot happen for a map decoded from JSON, and if it somehow does, an empty object here would
			// be the silent `{}` this package refuses elsewhere. Skipped instead, so the call is absent
			// rather than wrong.
			continue
		}
		out = append(out, map[string]any{
			"index": i, "id": tc.ID, "type": "function",
			"function": map[string]any{"name": tc.Name, "arguments": string(raw)},
		})
	}
	return out
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
	// FND-004's third state, about PROVENANCE rather than a value: present says we know whether these
	// counters were reported or derived, and absent says we are not claiming either.
	if u.Estimated != nil {
		usage["estimated"] = *u.Estimated
	}
	return response
}
