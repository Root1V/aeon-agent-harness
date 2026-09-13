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
