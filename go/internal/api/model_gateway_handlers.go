package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// ModelGatewayHandlers exposes the Model Gateway's routing/fallback (MDL-001) over HTTP — the only
// network-reachable way anything outside this process (the Python worker, starting with DR-001's
// Research Planner) can ask a provider to decide. Per docs/adr/0004: no caller other than this
// gateway ever talks to a provider SDK directly.
//
// Pricing/Ledger are OBS-003's FinOps addition, both optional and nil-safe: without them, /decide
// behaves exactly as before. With both configured, every successful call computes a real dollar
// cost from real token usage and a real config-as-code pricing table, and durably records it —
// best-effort (a ledger write failure is logged, never turned into a failed /decide response; cost
// observability must never be able to break the actual model call it's observing).
type ModelGatewayHandlers struct {
	Gateway *modelgateway.Gateway
	Pricing *finops.PricingTable
	Ledger  *store.FinOpsLedger
}

// Register mounts the model gateway routes on mux.
func (h *ModelGatewayHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /decide", h.decide)
}

type decideCandidate struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Priority int    `json:"priority"`
}

type decideRequest struct {
	Candidates      []decideCandidate `json:"candidates"`
	RenderedContext map[string]any    `json:"rendered_context"`
	DataSensitivity string            `json:"data_sensitivity,omitempty"`
	// RunID/AgentManifestRef (OBS-003, optional): tags a recorded cost event so it can later be
	// aggregated per run/agent, not just per model. No real caller populates these yet — see
	// backlog.md — so today every ledger row has both null.
	RunID            string `json:"run_id,omitempty"`
	AgentManifestRef string `json:"agent_manifest_ref,omitempty"`
}

func (h *ModelGatewayHandlers) decide(w http.ResponseWriter, r *http.Request) {
	var body decideRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(body.Candidates) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("candidates must be non-empty"))
		return
	}

	candidates := make([]modelgateway.Candidate, len(body.Candidates))
	for i, c := range body.Candidates {
		candidates[i] = modelgateway.Candidate{Provider: c.Provider, Model: c.Model, Priority: c.Priority}
	}

	result, err := h.Gateway.Decide(r.Context(), candidates, body.RenderedContext, body.DataSensitivity)
	if err != nil {
		// A gateway routing failure (every candidate failed, or a restricted call had none) is not
		// this handler's fault or the caller's malformed input — 502 Bad Gateway, not 400/500.
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	response := map[string]any{
		"provider_used": result.ProviderUsed,
		"model":         result.Model,
		"output":        result.Output,
		"attempts":      result.Attempts,
	}
	h.recordCost(r, result, body.RunID, body.AgentManifestRef, response)
	writeJSON(w, http.StatusOK, response)
}

// recordCost is OBS-003: computes a real dollar cost from this call's real token usage (when a
// rate is configured) and durably records it — best-effort, never blocking or failing the actual
// /decide response it's observing.
func (h *ModelGatewayHandlers) recordCost(r *http.Request, result *modelgateway.DecisionResult, runID, agentManifestRef string, response map[string]any) {
	if h.Pricing == nil {
		return
	}
	rate, ok := h.Pricing.Rate(result.ProviderUsed, result.Model)
	if !ok {
		return
	}

	promptTokens, completionTokens := usageTokens(result.Output)
	costUSD, priced := h.Pricing.CostUSD(result.ProviderUsed, result.Model, promptTokens, completionTokens)

	response["cost_model"] = rate.CostModel
	if priced {
		response["cost_usd"] = costUSD
	}

	if h.Ledger == nil {
		return
	}
	cacheRead, cacheWrite := usageCacheTokens(result.Output)
	reasoning := optionalInt(usageBlock(result.Output)["reasoning_tokens"])
	entry := store.CostEntry{
		Provider:         result.ProviderUsed,
		Model:            result.Model,
		CostModel:        rate.CostModel,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		CostUSD:          costUSD,
		RunID:            runID,
		AgentManifestRef: agentManifestRef,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
		ReasoningTokens:  reasoning,
	}
	if err := h.Ledger.Record(r.Context(), entry); err != nil {
		log.Printf("aeon-modelgw: recording FinOps cost event: %v", err)
	}
}

// usageTokens pulls prompt/completion token counts out of a NormalizedChatResponse's usage block,
// tolerating either real Go ints (the in-process case, providers.NormalizedChatResponse's own
// return type) or float64 (were this ever JSON-decoded first) — defensive, not a sign either shape
// is expected in practice.
func usageTokens(output map[string]any) (prompt, completion int) {
	usage, _ := output["usage"].(map[string]any)
	return toInt(usage["prompt_tokens"]), toInt(usage["completion_tokens"])
}

// usageCacheTokens reads MDL-012's cache counters, preserving the difference between a provider
// that reported nothing (key absent -> nil) and one that reported zero. Reading these with toInt
// like the other two counters would quietly turn every non-caching provider into a measured cold
// cache.
func usageCacheTokens(output map[string]any) (read, write *int) {
	usage := usageBlock(output)
	return optionalInt(usage["cache_read_tokens"]), optionalInt(usage["cache_write_tokens"])
}

func usageBlock(output map[string]any) map[string]any {
	usage, _ := output["usage"].(map[string]any)
	if usage == nil {
		return map[string]any{}
	}
	return usage
}

func optionalInt(v any) *int {
	if v == nil {
		return nil
	}
	n := toInt(v)
	return &n
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}
