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
func (g *Gateway) DecideStream(
	ctx context.Context, candidates []Candidate, renderedContext map[string]any, dataSensitivity string,
	yield func(providers.Chunk) error,
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

		spanCtx, span := tracer.Start(ctx, "chat", trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			providerNameAttr(c.Provider),
			attribute.String("gen_ai.request.model", c.Model),
			attribute.Bool("gen_ai.request.stream", true),
		))

		streamer, streamable := provider.(providers.StreamingProvider)
		delivered := false
		// The model a streamed response was served by arrives IN A CHUNK — StreamResult carries no
		// output — so it is captured on the way past. The last one seen wins, which is the same rule
		// the non-streamed path uses on the response body.
		servedModel := ""
		// Usage arrives in a chunk too, and usually in the LAST one — a provider reports it once the
		// generation is done. The last non-nil wins, and nil stays nil: a stream that reported no
		// usage must leave the attributes absent rather than claim zero (MDL-014).
		recordUnsendableDropped(span, unsendable)

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
