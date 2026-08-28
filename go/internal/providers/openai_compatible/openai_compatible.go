// Package openaicompatible is the openai_compatible adapter behind the Provider interface (docs/adr/0004). It is a
// stub: the real HTTP client, structured-output handling, and tool-calling translation are not
// yet implemented — see roadmap.md MDL-003..007.
package openaicompatible

import "context"

// Name identifies this adapter in ModelPolicyBundle candidates (proto/schemas/model_profile.schema.json).
const Name = "openai_compatible"

// Provider is the interface every model adapter implements. The Model Gateway (aeon-modelgw)
// depends only on this interface, never on a concrete provider SDK — see docs/adr/0004.
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
