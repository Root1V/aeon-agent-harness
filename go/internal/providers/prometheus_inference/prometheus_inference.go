// Package prometheusinference is the local-inference adapter behind the Provider interface
// (docs/adr/0004). Named prometheus_inference throughout config/code to avoid collision with
// Prometheus-the-metrics-system (prometheus_metrics), which is a separate part of this stack
// (see deploy/compose/docker-compose.yml, service "prometheus-metrics").
//
// STATUS: stub. Implemented against an assumed OpenAI-compatible API surface — see the open
// question in docs/adr/0004-model-gateway-provider-abstraction.md. If the user's Prometheus
// platform exposes a native API instead, this file is what changes.
package prometheusinference

import "context"

// Name identifies this adapter in ModelPolicyBundle candidates (proto/schemas/model_profile.schema.json).
const Name = "prometheus_inference"

// Provider is the interface every model adapter implements. The Model Gateway (aeon-modelgw)
// depends only on this interface, never on a concrete provider SDK — see docs/adr/0004.
type Provider interface {
	Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error)
	CachingCapability() string // expected "none" for most local setups — see docs/adr/0003.
	CostModel() string         // "compute_based" — billed by GPU-seconds, not tokens.
}
