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
	"encoding/json"
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

// New builds an Adapter over the Axonium SDK (MDL-009).
//
// authURL is accepted and IGNORED, on purpose, and the parameter stays so every caller does not have
// to change in the same commit that swaps the transport. The gateway serves /oauth2/token as well as
// /v1/ — verified on this deployment, both :8020 and :9000 answer a token request — so there is one
// address. Axonium removed the same second address from their SDK for the failure it caused: a client
// pointed at your own deployment but still minting tokens against the official platform, with nothing
// failing.
//
// clientID/clientSecret come from POST /admin/clients (issued once, cannot be retrieved again — treat
// as a secret, load from the environment, never hardcode). scope must include "inference:read" plus
// "model:<id>" for every model this adapter will request: being authorised for a model does not put it
// in the token, which is the 403 MDL-015 spent an afternoon on.
func New(authURL, gatewayURL, clientID, clientSecret, scope, model string) *Adapter {
	_ = authURL // see the doc above: one address, kept in the signature to avoid a wider change here
	return &Adapter{
		Model: model,
		Client: &Client{
			GatewayURL:   gatewayURL,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Scope:        scope,
		},
	}
}

// chatCompletionResponse is the (OpenAI-shaped) subset of the gateway's real response this adapter
// needs, decoded with real Go int fields — unlike Client.ChatCompletion's map[string]any, which
// decodes every JSON number as float64 (encoding/json's default for `any`) and would otherwise
// make this adapter's output inconsistent with every other Provider's NormalizedChatResponse.
type chatCompletionResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
			// MDL-016: reasoning models on this platform return their chain of thought here, and
			// dropping it makes a token-starved answer look like an empty one for no reason.
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		// Pointers (MDL-014): nil means the key was absent — including when the whole `usage` object is,
		// since then nothing sets them. That also covers the partial case, a usage object missing one
		// counter, which a pointer on the object alone would not.
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens *int `json:"completion_tokens"`
		// OpenAI's shape for the reasoning breakdown, which some servers mirror. A pointer so that
		// "not reported" stays distinct from "reasoned for zero tokens" all the way to the ledger.
		CompletionTokensDetails struct {
			ReasoningTokens *int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// Decide sends renderedContext as a Chat Completions request body, defaulting "model" to a.Model
// when the caller didn't already set one, and returns providers.NormalizedChatResponse — the same
// output shape every adapter returns, regardless of the fact that this provider's own wire format
// already happens to look similar.
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

	raw, meta, err := a.Client.ChatCompletionWithMeta(ctx, body)
	if err != nil {
		return nil, err
	}

	// Round-trip through JSON to get real int-typed fields — see chatCompletionResponse's doc.
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("prometheus_inference: re-encoding response: %w", err)
	}
	var parsed chatCompletionResponse
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		return nil, fmt.Errorf("prometheus_inference: decoding response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("prometheus_inference: response had no choices")
	}

	choice := parsed.Choices[0]
	return providers.NormalizedChatResponseFrom(providers.ChatResult{
		ProviderRequestID:  meta.RequestID,
		IdempotentReplayOf: meta.IdempotentReplayOf,
		ServedByInstance:   meta.InstanceID,
		Model:              parsed.Model,
		Content:            choice.Message.Content,
		ReasoningContent:   choice.Message.ReasoningContent,
		FinishReason:       choice.FinishReason,
		Usage: providers.Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			ReasoningTokens:  parsed.Usage.CompletionTokensDetails.ReasoningTokens,
		},
	}), nil
}

// CachingCapability: automatic_prefix — this platform DOES cache prompts, measured (MDL-009).
//
// automatic_prefix and not one of the other two documented values: the caching happens without being
// asked for, so there is nothing for the Budgeter to place breakpoints around (that is Anthropic's
// explicit_breakpoints). The value set is closed on purpose and the conformance suite enforces it —
// which is what caught the "prefix" I first wrote here without checking the list.
//
// It said "none" until now, on the reasoning that "most local-inference setups have no prompt
// caching". The deployment contradicts it plainly — a 12-token prompt came back with
// `prompt_tokens_details: {cached_tokens: 11}`, so 11 of 12 were served from cache:
//
//	usage: {prompt_tokens: 12, completion_tokens: 20, prompt_tokens_details: {cached_tokens: 11}}
//
// The direction of the old error is what made it worth fixing. ADR-003's Budgeter reads this to decide
// whether KEEPING context is cheaper than summarising it, and "none" pushed it toward aggressive
// offload — so the better the cache worked, the more we paid to avoid using it. Same shape as MDL-012,
// where the cache counters were invisible to the ledger: an error that grows as the optimisation
// succeeds, which is the kind nobody goes looking for.
func (a *Adapter) CachingCapability() string { return "automatic_prefix" }

// CostModel: token_based, measured (OBS-007), not GPU-seconds as this said before.
//
// The old value came from the project plan's assumption that self-hosted inference bills per GPU-second.
// GET /v1/usage/{request_id} returns the rates actually applied — 0.2 / 0.6 per 1M for qwen3-0.6b,
// 0.3067 / 1.052 for gpt-oss-20b-mxfp4 — and the cost recomputes from them exactly. OBS-007 corrected
// the bundle and left THIS declaration stale, which is the defect worth naming: the same fact was
// declared in two places and they disagreed for a day. The bundle is what pricing reads, so the
// disagreement was invisible; MDL-009's test is what surfaced it.
func (a *Adapter) CostModel() string { return "token_based" }
