// Package modelgateway is MDL-001: routes a capability profile to a concrete provider/model via
// ModelPolicyBundle-style candidates (proto/schemas/model_profile.schema.json), tries them in
// priority order, and falls back to the next candidate on any failure — the caller (a graph node,
// eventually) never sees a transient single-provider failure unless every candidate fails.
//
// The Gateway depends only on providers.Provider (go/internal/providers) — it never imports a
// concrete adapter package, matching docs/adr/0004: no agent code, and no gateway code, ever talks
// to a provider SDK directly.
package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/aeon-ai/aeon/go/internal/providers"
)

// tracer emits OBS-001's "chat" spans (OTel GenAI semantic conventions) around each provider
// attempt. A no-op until some binary calls tracing.Init (go/internal/tracing) — safe to use from
// any caller, instrumented or not.
var tracer = otel.Tracer("aeon-modelgw")

// Candidate mirrors one entry of ModelProfile's candidates[] (proto/schemas/model_profile.schema.json).
type Candidate struct {
	Provider string
	Model    string
	Priority int // lower tries first
	// InNetwork says this candidate's provider is one the profile declared as in-network, which is
	// what data_sensitivity=restricted filters on. Resolved from the bundle rather than decided
	// here: which providers never leave the network is a fact about a deployment.
	InNetwork bool
}

// ErrAllCandidatesFailed wraps the per-candidate attempt log when every candidate in a Decide call
// failed (including "provider not registered").
var ErrAllCandidatesFailed = errors.New("modelgateway: all candidates failed")

// ErrNoRestrictedCandidate is returned when dataSensitivity="restricted" is requested but no
// candidate is served by a provider the profile declares as in-network — restricted data must never
// fall through to a provider outside the network just because none was configured (ADR-004).
//
// Note what this deliberately does NOT do: name a provider. Which providers are in-network is a
// property of a deployment, not of a harness, so it is declared in the ModelPolicyBundle and read
// from there (MDL-017). An earlier version had "prometheus_inference" as a constant in this file —
// it honoured ADR-004's letter (this package imports no adapter) while breaking its point, because
// a routing core that knows one platform's name by heart is coupled to it whether it imports the
// package or not.
var ErrNoRestrictedCandidate = errors.New("modelgateway: data_sensitivity=restricted requires a candidate from a provider the profile declares in-network, none configured")

// AttemptRecord logs one candidate's outcome, successful or not — this is what makes routing
// decisions observable rather than a black box, and is exactly what the future OBS-001 tracing
// span for a model call should carry.
type AttemptRecord struct {
	Provider string
	Model    string
	Err      string // empty means this attempt succeeded
}

// DecisionResult is what a caller gets back: which provider/model actually served the call, its
// raw (adapter-normalized) output, and the full attempt log — including failed attempts before
// the one that succeeded.
type DecisionResult struct {
	ProviderUsed string
	Model        string
	Output       map[string]any
	Attempts     []AttemptRecord
}

// QualityGate is MDL-002's quality-aware routing hook: given a candidate about to be tried,
// reports whether its recent eval score has degraded below its configured threshold. Decide skips
// (never even attempts) a degraded candidate, falling through to the next one exactly like an
// unregistered provider does today — this package defines the interface it needs (Go convention);
// go/internal/store.QualityScoreStore satisfies it structurally, with no import from this package
// to that one.
type QualityGate interface {
	IsDegraded(ctx context.Context, provider, model string) bool
}

// Gateway holds registered provider adapters, keyed by whatever name the bundle uses for them. The
// names are data: this package never compares one against a literal.
type Gateway struct {
	providers map[string]providers.Provider
	// Quality is optional and nil-safe (MDL-002): nil means no quality gating at all, identical to
	// behavior before this field existed.
	Quality QualityGate
}

// New returns an empty Gateway; register providers with RegisterProvider before calling Decide.
func New() *Gateway {
	return &Gateway{providers: map[string]providers.Provider{}}
}

// RegisterProvider adds (or replaces) a provider adapter under the given name.
func (g *Gateway) RegisterProvider(name string, p providers.Provider) {
	g.providers[name] = p
}

// Decide tries candidates in ascending Priority order (independent of slice order), falling back
// to the next on any error. dataSensitivity, when "restricted", filters candidates down to only
// the prometheus_inference one(s) before routing even begins — a cloud candidate is never even
// attempted for restricted data, not just deprioritized.
func (g *Gateway) Decide(
	ctx context.Context, candidates []Candidate, renderedContext map[string]any, dataSensitivity string,
) (*DecisionResult, error) {
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

		spanCtx, span := tracer.Start(ctx, "chat", trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			providerNameAttr(c.Provider),
			attribute.String("gen_ai.request.model", c.Model),
		))
		output, err := provider.Decide(spanCtx, input)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			span.End()
			attempts = append(attempts, AttemptRecord{Provider: c.Provider, Model: c.Model, Err: err.Error()})
			continue
		}
		// A 200 is not the same as an answer (MDL-015). A response with no content and no tool calls
		// is unusable by any caller, so it is treated as this candidate failing and the cascade moves
		// on — which is what the cascade is for.
		//
		// Measured against the real deployment: gpt-oss-20b-mxfp4, a reasoning model, answers the
		// Deep Research Researcher prompt with finish_reason "stop", a short reasoning_content and
		// content "" — everything went to its analysis channel and the answer channel stayed empty.
		// qwen3-0.6b answers the same prompt with valid JSON 4 times out of 4. Without this, the
		// gateway returns the empty answer, the caller's parser fails, and the SECOND candidate --
		// the one that works -- is never tried.
		//
		// finish_reason "length" is deliberately excluded: that is a real, informative outcome the
		// caller must see. Retrying it on another model would hide a budget the caller needs to raise.
		if unusable := emptyAnswer(output); unusable != "" {
			span.SetStatus(codes.Error, unusable)
			span.End()
			attempts = append(attempts, AttemptRecord{Provider: c.Provider, Model: c.Model, Err: unusable})
			continue
		}
		recordResponseModel(span, responseModelOf(output))
		inputTokens, outputTokens := usageOf(output)
		recordUsage(span, inputTokens, outputTokens)
		span.SetStatus(codes.Ok, "")
		span.End()

		attempts = append(attempts, AttemptRecord{Provider: c.Provider, Model: c.Model})
		return &DecisionResult{ProviderUsed: c.Provider, Model: c.Model, Output: output, Attempts: attempts}, nil
	}

	return nil, fmt.Errorf("%w: %+v", ErrAllCandidatesFailed, attempts)
}

// restrictPool narrows the candidates to those the profile declares in-network when the call is
// marked restricted. A candidate carries that declaration itself (Candidate.InNetwork), resolved
// from the bundle — so the rule is enforced here and decided there.
//
// Failing closed is the point: no declaration means no candidate qualifies, which is an error
// rather than a quiet fall-through to whatever was configured.
func (g *Gateway) restrictPool(candidates []Candidate, dataSensitivity string) ([]Candidate, error) {
	if dataSensitivity != "restricted" {
		return candidates, nil
	}
	var out []Candidate
	for _, c := range candidates {
		if c.InNetwork {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, ErrNoRestrictedCandidate
	}
	return out, nil
}

// emptyAnswer reports why a normalized response is unusable, or "" when it is fine.
//
// "Unusable" is narrow on purpose: no content, no tool calls, and a finish_reason that is not
// "length". A tool-call-only response has empty content and IS usable, so it must not be caught here;
// and a truncated response is informative, so it must reach the caller.
func emptyAnswer(output map[string]any) string {
	choices, _ := output["choices"].([]any)
	if len(choices) == 0 {
		return "provider returned no choices"
	}
	choice, _ := choices[0].(map[string]any)
	if fr, _ := choice["finish_reason"].(string); fr == "length" {
		return ""
	}
	message, _ := choice["message"].(map[string]any)
	if content, _ := message["content"].(string); strings.TrimSpace(content) != "" {
		return ""
	}
	if calls, _ := message["tool_calls"].([]any); len(calls) > 0 {
		return ""
	}
	if reasoning, _ := message["reasoning_content"].(string); strings.TrimSpace(reasoning) != "" {
		return "provider returned only reasoning and no answer"
	}
	return "provider returned empty content"
}

// responseModelOf reads the model the provider says answered, out of the normalized response.
func responseModelOf(output map[string]any) string {
	served, _ := output["model"].(string)
	return served
}

// recordResponseModel puts OTel GenAI's `gen_ai.response.model` on the span beside the
// `gen_ai.request.model` already there, and marks a divergence when the two differ.
//
// WHY THE PAIR IS WORTH ANYTHING HERE AND NOT IN A GATEWAY, which is the argument Argus's own P-21
// makes in the other direction: from a gateway both attributes come from the same already-resolved
// variable, so they can never disagree and the pair cannot detect anything. From a CLIENT they come
// from two different places — the name this gateway picked out of the ModelPolicyBundle, and the name
// that came back in the response body — so a disagreement is observable. That makes this an
// independent second witness of a discrepancy the gateway structurally cannot report.
//
// AND THE TWO DISAGREE FOR TWO UNRELATED REASONS, both of which matter:
//
//   - BY DESIGN. A caller asks for a capability profile and never a model (docs/adr/0004), so
//     routing and fallback mean "what was asked" and "what answered" are different things. A silent
//     fall-through to a local candidate is the case that costs money: MDL-018 exists because local
//     inference is never priced, so the moment routing falls there a dollar ceiling stops capping
//     anything.
//   - BECAUSE THE PLATFORM SERVED SOMETHING ELSE. OBS-005's case, measured by Axonium against an
//     instance-specific name, and the shape OBS-008 came from — the platform changed what model_id
//     meant in its usage export, and a model with no configured rate left NO ledger row at all. A
//     rename shows up as request != response before it shows up as a hole in the books.
//
// ABSENT WHEN THE PROVIDER REPORTED NONE, not set equal to the request. Three states: present and
// equal, present and different, absent because nothing said. Defaulting an absent value to the
// requested model would manufacture agreement, which is the one answer this pair must never give.
//
// TWO CONVENTION ATTRIBUTES AND NOTHING OF OUR OWN, and the first version of this function had a
// third. It set `aeon.model.response_differs` on a divergence, with a comment arguing that the pair
// alone makes a divergence *recorded* and not *findable* because TraceQL compares an attribute
// against a literal rather than against another attribute.
//
// THAT CLAIM WAS FALSE, measured against a real Tempo in TestADivergentResponseModelIsFindable:
// `{ span.gen_ai.request.model != span.gen_ai.response.model }` is accepted and matches. And the
// absent state does not produce a false positive — a span carrying no response model is NOT matched
// by that query, measured with a span that arrived and whose attribute is genuinely missing. So the
// flag was redundant, it was a non-standard attribute for Argus's semconv suite to reject, and it
// existed because I asserted something about a query language instead of asking it.
func recordResponseModel(span trace.Span, served string) {
	if served == "" {
		return
	}
	span.SetAttributes(attribute.String("gen_ai.response.model", served))
}

// providerNameAttr emits `gen_ai.provider.name` and NOT the deprecated `gen_ai.system`.
//
// WE REPORTED THIS DEFECT TO SOMEBODY ELSE AND HAD IT OURSELVES. VRT-AXO-002 is Veritium asking
// Axonium to stop emitting `gen_ai.system` because the convention deprecated it; Axonium delivered
// it across three SDKs and asserted the old attribute ABSENT rather than only the new one present,
// with the argument that "un emisor que mandara los dos pasaría cualquier test que solo comprobara
// el nuevo". These two spans kept emitting it the whole time. Found while adding the usage
// attributes, by reading what that entry actually agreed to.
//
// SO IT IS A REPLACEMENT AND NOT AN ADDITION, for exactly that reason, and the test asserts the old
// name is gone.
//
// THE VALUE IS OUR ADAPTER NAME (prometheus_inference, openai, anthropic...) and not an inference
// engine. Argus's A-10 maps this attribute to the engine (llama.cpp, vllm, ollama), which is right
// for a span emitted BY a gateway that knows its backend and wrong for one emitted by a client that
// does not: we would have to guess. Axonium reached the same conclusion for the same reason and
// emits their own `prometheus-gateway`; the convention permits a custom value when no known one
// applies, and a true custom value beats a guessed standard one.
func providerNameAttr(provider string) attribute.KeyValue {
	return attribute.String("gen_ai.provider.name", provider)
}

// usageOf reads the two base token counters out of a normalized response, PRESERVING ABSENCE.
//
// Both are pointers because providers.usageBlock OMITS a counter the provider did not report
// (MDL-014), and the distinction has to survive all the way here: a provider that reported nothing
// and one that genuinely used zero tokens are different facts.
func usageOf(output map[string]any) (input, out *int) {
	usage, _ := output["usage"].(map[string]any)
	return intFrom(usage["prompt_tokens"]), intFrom(usage["completion_tokens"])
}

// intFrom tolerates both a real int (the in-process case, which is what a Provider returns) and a
// float64 (were the response ever JSON-decoded first). Defensive rather than expected, and the same
// tolerance model_gateway_handlers.optionalInt already applies for the ledger.
func intFrom(v any) *int {
	switch n := v.(type) {
	case int:
		return &n
	case int64:
		asInt := int(n)
		return &asInt
	case float64:
		asInt := int(n)
		return &asInt
	}
	return nil
}

// recordUsage puts OTel GenAI's two token counters on the span.
//
// ABSENT WHEN THE PROVIDER REPORTED NOTHING, never zero. This is the same rule MDL-014 established
// for the ledger and OBS-008 for the dashboard, and the reason it matters on a span is that a trace
// is where somebody goes to ask "what did this call cost": a fabricated 0 reads as a measured zero
// and sums into a total that looks exact. An absent attribute is at worst ambiguous; a zero is a
// positive claim nothing supports.
//
// THE NAMES ARE THE CURRENT ONES. `gen_ai.usage.input_tokens` / `output_tokens` replaced
// `prompt_tokens` / `completion_tokens` in the convention — the normalized response still carries
// the old spellings because that is OpenAI's wire format and INT-002's consumers depend on it, so
// this is the boundary where the two vocabularies meet and the mapping is explicit rather than
// implied by a matching name.
func recordUsage(span trace.Span, input, output *int) {
	if input != nil {
		span.SetAttributes(attribute.Int("gen_ai.usage.input_tokens", *input))
	}
	if output != nil {
		span.SetAttributes(attribute.Int("gen_ai.usage.output_tokens", *output))
	}
}
