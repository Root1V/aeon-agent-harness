package providers

import "context"

// Chunk is one incremental piece of a streamed model response, normalized the same way
// NormalizedChatResponse normalizes a complete one: a caller reads Delta the same way regardless of
// whether the adapter behind it speaks OpenAI's SSE, Anthropic's event stream, or Gemini's.
//
// Usage is populated only on the final chunk, and only when the provider actually reports it —
// several OpenAI-compatible servers omit usage while streaming unless explicitly asked (see
// go/internal/providers/openai_compatible's stream_options handling). A nil Usage means "not
// reported", never "zero": FinOps (OBS-003) must not record a fabricated 0 for a streamed call.
type Chunk struct {
	Delta string
	// ReasoningDelta carries chain-of-thought tokens, which arrive interleaved with — and before —
	// the answer. It is a separate field because the shared normalization contract is explicit that
	// these are two flows and must never be concatenated: a consumer showing a live answer would
	// otherwise print the model's deliberation as if it were the reply.
	ReasoningDelta string
	FinishReason   string
	Model          string
	Usage          *Usage

	// Event is the lifecycle marker of the shared normalization contract (FND-004): a uniform
	// start/delta/end cycle for text, reasoning and tool calls, with `finish` always last.
	//
	// Why a lifecycle at all, when the deltas alone carry the content: an END is information the deltas
	// cannot express. The contract's own example is that the reasoning phase closes when the tool call
	// OPENS, not when the stream ends — a consumer watching only deltas cannot tell those apart, and one
	// rendering live output would leave the deliberation on screen for the rest of the turn.
	//
	// Empty on a chunk that is not a lifecycle marker, so an adapter that does not emit the cycle keeps
	// working exactly as before.
	Event StreamEvent
	// ToolCallIndex correlates the fragments of one tool call. The wire interleaves several calls and
	// identifies them ONLY by this index; matching on name would fail because the name arrives in the
	// first fragment and not the rest.
	ToolCallIndex int
	// ToolCallDelta is a raw fragment of the arguments JSON. NOT parseable on its own — the contract is
	// explicit that an adapter trying to parse each fragment produces errors on the happy path.
	ToolCallDelta string
	// ToolCall is set ONLY on EventToolCallEnd, with arguments parsed once, from the accumulated
	// fragments. One parse per call, at the one moment the text is complete.
	ToolCall *ToolCall
}

// StreamEvent names a point in the streaming lifecycle.
type StreamEvent string

const (
	EventStreamStart    StreamEvent = "stream_start"
	EventTextStart      StreamEvent = "text_start"
	EventTextDelta      StreamEvent = "text_delta"
	EventTextEnd        StreamEvent = "text_end"
	EventReasoningStart StreamEvent = "reasoning_start"
	EventReasoningDelta StreamEvent = "reasoning_delta"
	EventReasoningEnd   StreamEvent = "reasoning_end"
	EventToolCallStart  StreamEvent = "tool_call_start"
	EventToolCallDelta  StreamEvent = "tool_call_delta"
	EventToolCallEnd    StreamEvent = "tool_call_end"
	// EventFinish is ALWAYS the last event, carrying the finish reason and the usage. The contract
	// requires it so a consumer that read the deltas does not have to reassemble the response itself.
	EventFinish StreamEvent = "finish"
)

// CarriesWireContent reports whether this chunk has anything an OpenAI-compatible client expects to see.
//
// Used by the SSE surface (INT-002) to skip pure lifecycle markers. Without it, adding the lifecycle
// would have started emitting empty `data:` frames to every existing client of that endpoint — a real
// change to a surface whose entire purpose is being unsurprising. The events are Aeon's internal
// vocabulary; the wire format is not ours to extend.
func (c Chunk) CarriesWireContent() bool {
	return c.Delta != "" || c.ReasoningDelta != "" || c.FinishReason != "" || c.Usage != nil
}

// Usage is the token accounting a provider reports.
//
// PromptTokens follows the convention agreed with the Synaptum and Axonium teams: it is the TOTAL
// input, cached tokens included, and CacheReadTokens says how many of them were served from cache.
// A consumer can therefore read PromptTokens without knowing which provider produced it. Note what
// that costs: the convention was argued on the grounds that adapters copy rather than compute, and
// that is true of OpenAI-shaped providers but not of Anthropic, which reports the two counters
// disjointly — see the anthropic adapter, which must add.
//
// CacheReadTokens and CacheWriteTokens are pointers because three states matter and two would lose
// one: nil means the provider does not report caching at all, while a zero means it reported that
// nothing was cached. Collapsing them turns an unmeasured cache into a cold one, and FinOps would
// record a fabricated fact.
//
// CacheWriteTokens is NOT part of PromptTokens, by the same agreement. Reasoning tokens, the fifth
// field of decision H3, still belong with the shared vocabulary (FND-004) — the cache pair is here
// now because its absence is a live undercount, not a missing feature.
type Usage struct {
	// PromptTokens/CompletionTokens are POINTERS since MDL-014, completing what MDL-012 and MDL-016
	// started: nil means the provider reported no usage at all, which is not a call that consumed
	// nothing. A response with no `usage` object normalized to 0 before this, and the ledger recorded a
	// 0-token, $0 call — the same fabricated fact the cache counters had, on the two counters everything
	// reads.
	//
	// Found by RUNNING Synaptum's normalization corpus, not by reading code: it was the last real
	// divergence of the ten cases.
	PromptTokens     *int
	CompletionTokens *int
	CacheReadTokens  *int
	CacheWriteTokens *int
	// ReasoningTokens is the part of the output spent thinking rather than answering (MDL-016).
	// Pointer for the same three-state reason as the cache counters: a provider that does not break
	// it out is not a provider that reasoned for free.
	ReasoningTokens *int
	// Estimated is the contract's third state about PROVENANCE, not about a value: nil says nothing,
	// false means the provider reported these counters, and true means they were derived (from
	// llama.cpp's `timings`, today). A bool would make every unreported usage look reported, which is the
	// same collapse the counters themselves already avoid.
	Estimated *bool
}

// StreamingProvider is an OPTIONAL capability on top of Provider. An adapter that implements it can
// deliver a response incrementally and — crucially — stop generating when the caller goes away.
//
// The yield-callback shape (rather than a channel or an iterator) is deliberate: it makes
// cancellation the caller's decision and gives it a single, obvious mechanism. Returning a non-nil
// error from yield means "stop now"; the adapter must abandon the upstream response and return,
// which closes the connection and lets the real provider observe the abort. That is the whole
// point of INT-008 — a stream you can only discard after receiving it entirely is not cancellable,
// and the hot budget cutoff every layer above depends on (A6, and the tripartite agreement's P5)
// needs the abort to reach the actual model, not just the local reader.
//
// The gateway falls back to Provider.Decide for any adapter that does not implement this, emitting
// the whole response as a single chunk. That keeps the streaming endpoint usable against every
// provider, at the honest cost that such a call cannot be cancelled mid-generation.
type StreamingProvider interface {
	Provider
	DecideStream(ctx context.Context, renderedContext map[string]any, yield func(Chunk) error) error
}
