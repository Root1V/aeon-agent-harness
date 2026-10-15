package modelgateway

import (
	"context"
	"fmt"
	"sort"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/aeon-ai/aeon/go/internal/providers"
)

// StreamResult reports how a streamed call was actually served, once it finished or was cut short.
type StreamResult struct {
	ProviderUsed string
	Model        string
	// Streamed is false when the chosen provider had no streaming support and the gateway fell
	// back to a single-shot call delivered as one chunk — such a call cannot be cancelled
	// mid-generation, and a caller that cares (a budget enforcer) needs to know which it got.
	Streamed bool
	Attempts []AttemptRecord
	// UnsendableTurnsDropped counts assistant turns this gateway removed from the request because
	// nothing on the chat wire could carry them (VRT-SYN-004 — see unsendable.go). Reported rather
	// than silent: dropping a message narrows the request the caller built, and a gateway that
	// narrows requests without saying so is the defect VRT-AEON-003 was raised about.
	UnsendableTurnsDropped int
}

// DecideStream routes exactly like Decide — restricted-data filtering, priority order, quality
// gating (MDL-002), fallback on failure — and streams the chosen provider's response through yield.
//
// One rule that does not exist in Decide: **fallback stops once the first chunk is delivered.**
// After the caller has bytes, silently retrying another candidate would splice two different
// models' output into one response. A failure after that point is returned as-is.
//
// Cancellation (INT-008, and the tripartite agreement's P5) propagates by construction: yield's
// error stops the adapter, which closes its upstream connection, which is what makes the real model
// stop generating. Nothing here waits for a complete response before honoring a cut.
// StreamStart is what the gateway knows BEFORE the first chunk, for a caller that has to put it in
// response headers (OBS-018).
//
// WHY A CALLBACK AND NOT THE RETURN VALUE. Everything here is also on StreamResult, which arrives
// when the stream is over — and by then the caller has written chunks, so WriteHeader has happened
// and headers have stopped being writable. Go ignores a later Header().Set in silence, which is
// exactly how `X-Aeon-Streamed: false` came to be set on every single-chunk response and reach no
// client at all.
//
// CALLED ONCE PER CANDIDATE ATTEMPT, not once per call, and that is deliberate: a candidate may fail
// before producing any chunk, the gateway falls through to the next one, and the caller must be able
// to correct what it would have sent. Setting a header is free until the first write, so the last
// StreamStart before that write is the one the client sees. A candidate that fails AFTER emitting
// chunks cannot be corrected — the headers are already on the wire — and that is inherent to
// streaming rather than something this hook could fix.
type StreamStart struct {
	Provider string
	Model    string
	// Streamed is false when this candidate has no streaming support and its answer will be
	// delivered as one chunk.
	Streamed bool
	// UnsendableTurnsDropped is VRT-SYN-004's count for this attempt. It shares this hook because it
	// has the same shape of problem: known before the provider is called, needed in a header.
	UnsendableTurnsDropped int
}

// DecideStream routes exactly like Decide and streams the chosen provider's answer through yield.
// onStart, when non-nil, is called before each candidate is invoked — see StreamStart.
func (g *Gateway) DecideStream(
	ctx context.Context, candidates []Candidate, renderedContext map[string]any, dataSensitivity string,
	yield func(providers.Chunk) error, onStart func(StreamStart),
) (*StreamResult, error) {
	pool, err := g.restrictPool(candidates, dataSensitivity)
	if err != nil {
		return nil, err
	}

	sorted := make([]Candidate, len(pool))
	copy(sorted, pool)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })

	var attempts []AttemptRecord
	for _, c := range sorted {
		if g.Quality != nil && g.Quality.IsDegraded(ctx, c.Provider, c.Model) {
			attempts = append(attempts, AttemptRecord{Provider: c.Provider, Model: c.Model, Err: "skipped: quality score degraded below threshold"})
			continue
		}

		provider, ok := g.providers[c.Provider]
		if !ok {
			attempts = append(attempts, AttemptRecord{Provider: c.Provider, Model: c.Model, Err: "provider not registered"})
			continue
		}

		input := make(map[string]any, len(renderedContext)+1)
		for k, v := range renderedContext {
			input[k] = v
		}
		input["model"] = c.Model
		// VRT-SYN-004, and before the span opens so the attribute below describes what was sent.
		unsendable := dropUnsendableAssistantTurns(input)

		// `{operation} {model}` — the span NAME the ecosystem agreed in VRT-AXO-002 (OBS-015), not
		// `chat` alone. Argus asked for the standard name explicitly when Prometheus offered to step
		// away from it over cardinality ("seguid el estándar, la cardinalidad es nuestro problema"),
		// and they cleared the cut in ARG-AXO-001 by measuring zero panels or rules filtering on the
		// old name.
		//
		// THE REQUEST MODEL AND NOT THE SERVED ONE, so the name matches `gen_ai.request.model` beside
		// it. The served model may differ — that is what OBS-012's pair exists to make findable — and
		// naming the span after it would make a span's identity depend on the answer, so two calls
		// asking the same thing would land under different names.
		//
		// OUR PYTHON HALF ALREADY DID THIS: argus's own `genai()` builds
		// `f"{operation} {request_model}"`, so until this change one trace carried an activity span
		// named `chat qwen3-0.6b` and a gateway span named `chat`. The inconsistency was inside a
		// single trace of ours, not just against the convention.
		spanCtx, span := tracer.Start(ctx, "chat "+c.Model, trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			providerNameAttr(c.Provider),
			attribute.String("gen_ai.request.model", c.Model),
			attribute.Bool("gen_ai.request.stream", true),
		))
		recordServerAddress(span, provider)

		streamer, streamable := provider.(providers.StreamingProvider)
		recordUnsendableDropped(span, unsendable)
		// BEFORE the provider is invoked, because that is the last moment a caller can still set a
		// response header: the first chunk triggers WriteHeader and anything set after it is dropped
		// in silence (OBS-018).
		if onStart != nil {
			onStart(StreamStart{
				Provider: c.Provider, Model: c.Model, Streamed: streamable,
				UnsendableTurnsDropped: unsendable,
			})
		}
		delivered := false
		// The model a streamed response was served by arrives IN A CHUNK — StreamResult carries no
		// output — so it is captured on the way past. The last one seen wins, which is the same rule
		// the non-streamed path uses on the response body.
		servedModel := ""
		// Usage arrives in a chunk too, and usually in the LAST one — a provider reports it once the
		// generation is done. The last non-nil wins, and nil stays nil: a stream that reported no
		// usage must leave the attributes absent rather than claim zero (MDL-014).

		var inputTokens, outputTokens *int
		wrapped := func(chunk providers.Chunk) error {
			delivered = true
			if chunk.Model != "" {
				servedModel = chunk.Model
			}
			if chunk.Usage != nil {
				if chunk.Usage.PromptTokens != nil {
					inputTokens = chunk.Usage.PromptTokens
				}
				if chunk.Usage.CompletionTokens != nil {
					outputTokens = chunk.Usage.CompletionTokens
				}
			}
			return yield(chunk)
		}

		var err error
		if streamable {
			err = streamer.DecideStream(spanCtx, input, wrapped)
		} else {
			err = decideAsSingleChunk(spanCtx, provider, input, wrapped)
		}

		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			span.End()
			attempts = append(attempts, AttemptRecord{Provider: c.Provider, Model: c.Model, Err: err.Error()})
			if delivered {
				// Already streaming to the caller: no other candidate can take over cleanly.
				return &StreamResult{ProviderUsed: c.Provider, Model: c.Model, Streamed: streamable, Attempts: attempts}, err
			}
			continue
		}

		recordResponseModel(span, servedModel)
		recordUsage(span, inputTokens, outputTokens)
		span.SetStatus(codes.Ok, "")
		span.End()
		attempts = append(attempts, AttemptRecord{Provider: c.Provider, Model: c.Model})
		return &StreamResult{
			ProviderUsed: c.Provider, Model: c.Model, Streamed: streamable, Attempts: attempts,
			UnsendableTurnsDropped: unsendable,
		}, nil
	}

	return nil, fmt.Errorf("%w: %+v", ErrAllCandidatesFailed, attempts)
}

// decideAsSingleChunk serves a streaming request from a non-streaming adapter: one call, one chunk,
// with the usage the adapter reported. Honest degradation rather than refusing the provider — but
// note that ctx cancellation here can only abort the request, never a generation already in flight
// on the provider's side, which is exactly the difference StreamResult.Streamed records.
func decideAsSingleChunk(
	ctx context.Context, provider providers.Provider, input map[string]any, yield func(providers.Chunk) error,
) error {
	output, err := provider.Decide(ctx, input)
	if err != nil {
		return err
	}

	chunk := providers.Chunk{}
	if model, ok := output["model"].(string); ok {
		chunk.Model = model
	}
	if choices, ok := output["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				chunk.Delta, _ = message["content"].(string)
			}
			chunk.FinishReason, _ = choice["finish_reason"].(string)
		}
	}
	if usage, ok := output["usage"].(map[string]any); ok {
		chunk.Usage = &providers.Usage{
			PromptTokens:     optionalIntFrom(usage["prompt_tokens"]),
			CompletionTokens: optionalIntFrom(usage["completion_tokens"]),
		}
	}
	return yield(chunk)
}

// optionalIntFrom reads a token count out of a decoded JSON map, where numbers arrive as float64 unless
// the adapter built the map with real ints (both happen — see providers.NormalizedChatResponse).
//
// It returns nil for an absent key, and that replaced a `default: return 0` which was MDL-014's defect
// in miniature: a counter the provider never reported came out of here as a measured zero, and the
// caller had no way to tell. NormalizedChatResponse now OMITS a counter it was not given, so absence is
// the normal case here rather than an oddity.
func optionalIntFrom(v any) *int {
	switch n := v.(type) {
	case int:
		return &n
	case float64:
		i := int(n)
		return &i
	default:
		return nil
	}
}
