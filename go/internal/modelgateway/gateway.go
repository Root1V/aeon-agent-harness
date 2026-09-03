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
}

// ErrAllCandidatesFailed wraps the per-candidate attempt log when every candidate in a Decide call
// failed (including "provider not registered").
var ErrAllCandidatesFailed = errors.New("modelgateway: all candidates failed")

// ErrNoRestrictedCandidate is returned when dataSensitivity="restricted" is requested but no
// candidate names the prometheus_inference provider — restricted data must never fall through to
// a cloud provider just because no local candidate was configured (ADR-004).
var ErrNoRestrictedCandidate = errors.New("modelgateway: data_sensitivity=restricted requires a prometheus_inference candidate, none configured")

// restrictedProvider is the only provider allowed when a call is marked data_sensitivity=restricted
// (proto/schemas/model_profile.schema.json's routing_constraints) — sensitive data must never
// leave the local network.
const restrictedProvider = "prometheus_inference"

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

// Gateway holds registered provider adapters, keyed by name (proto/schemas/model_profile.schema.json's
// candidates[].provider enum: anthropic, openai, gemini, prometheus_inference, openai_compatible).
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
	pool := candidates
	if dataSensitivity == "restricted" {
		pool = filterByProvider(candidates, restrictedProvider)
		if len(pool) == 0 {
			return nil, ErrNoRestrictedCandidate
		}
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
			attribute.String("gen_ai.system", c.Provider),
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
		span.SetStatus(codes.Ok, "")
		span.End()

		attempts = append(attempts, AttemptRecord{Provider: c.Provider, Model: c.Model})
		return &DecisionResult{ProviderUsed: c.Provider, Model: c.Model, Output: output, Attempts: attempts}, nil
	}

	return nil, fmt.Errorf("%w: %+v", ErrAllCandidatesFailed, attempts)
}

func filterByProvider(candidates []Candidate, provider string) []Candidate {
	var out []Candidate
	for _, c := range candidates {
		if c.Provider == provider {
			out = append(out, c)
		}
	}
	return out
}
