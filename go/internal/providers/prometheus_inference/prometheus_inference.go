// Package prometheusinference is the local-inference adapter behind the Provider interface
// (go/internal/providers, docs/adr/0004). Named prometheus_inference throughout config/code to
// avoid collision with Prometheus-the-metrics-system (prometheus_metrics), which is a separate
// part of this stack (see deploy/compose/docker-compose.yml, service "prometheus-metrics").
//
// Real, not a stub: OAuth2 client_credentials against the platform's auth-service (auth.go) and
// an OpenAI-Chat-Completions-compatible gateway client (client.go), confirmed directly against the
// Prometheus platform team's own integration docs (docs/adr/0004's former "open question" — now
// resolved there).
package prometheusinference

import (
	"context"
	"fmt"

	"github.com/aeon-ai/aeon/go/internal/providers"
)

// Name identifies this adapter in ModelPolicyBundle candidates (proto/schemas/model_profile.schema.json).
const Name = "prometheus_inference"

// Provider is an alias for the shared interface (go/internal/providers) every model adapter
// implements — kept here so existing references to prometheusinference.Provider keep working.
type Provider = providers.Provider

// Adapter implements Provider against a real Prometheus gateway + auth-service pair.
type Adapter struct {
	Model  string // the model id to request, e.g. "llama3-8b-q4-local" — must be in the client's granted scope
	Client *Client
}

// New builds an Adapter with its own TokenSource, wired to authURL/gatewayURL. clientID/
// clientSecret come from POST /admin/clients (issued once, cannot be retrieved again — treat as a
// secret, load from environment/`.env`, never hardcode). scope must include "inference:read" (and
// "inference:stream" if streaming is used later) plus "model:<id>" for every model this adapter
// will request.
func New(authURL, gatewayURL, clientID, clientSecret, scope, model string) *Adapter {
	tokens := &TokenSource{
		AuthURL:      authURL,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scope:        scope,
	}
	return &Adapter{
		Model:  model,
		Client: &Client{GatewayURL: gatewayURL, Tokens: tokens},
	}
}

// Decide sends renderedContext as a Chat Completions request body, defaulting "model" to a.Model
// when the caller didn't already set one, and returns the raw response for the gateway to turn
// into a typed Decision (proto/schemas/decision.schema.json).
func (a *Adapter) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	body := make(map[string]any, len(renderedContext)+1)
	for k, v := range renderedContext {
		body[k] = v
	}
	if _, hasModel := body["model"]; !hasModel {
		if a.Model == "" {
			return nil, fmt.Errorf("prometheus_inference: no model specified in renderedContext and no default Model configured")
		}
		body["model"] = a.Model
	}
	return a.Client.ChatCompletion(ctx, body)
}

// CachingCapability: most local-inference setups have no prompt caching (ADR-003's Budgeter falls
// back to aggressive offload for this provider). Not something the gateway's API surfaces either
// way today.
func (a *Adapter) CachingCapability() string { return "none" }

// CostModel: self-hosted inference is billed by GPU-seconds, not tokens (FinOps, OBS-003).
func (a *Adapter) CostModel() string { return "compute_based" }
