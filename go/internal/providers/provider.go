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
