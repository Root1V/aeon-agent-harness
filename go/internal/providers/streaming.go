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
	Delta        string
	FinishReason string
	Model        string
	Usage        *Usage
}

// Usage is the token accounting a provider reports. Deliberately the same two counters
// NormalizedChatResponse already emits, so the FinOps ledger consumes a streamed call exactly like
// a non-streamed one. Extending this to the tripartite agreement's five-field shape
// (input/output/reasoning/cache_read/cache_write, decision H3) belongs with the shared vocabulary
// (FND-004), not here — doing it now would change what OBS-003's ledger reads for every caller.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
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
