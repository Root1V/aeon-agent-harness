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
//
// OBS-008 changed the control flow here, and the change is the whole point. This used to return
// early when Pricing.Rate found nothing, which was *before* the ledger write: a model with no
// configured rate — a renamed one, most likely — left no row at all. Not a row with a null price,
// which an audit can find and ask about, but nothing, which an audit cannot distinguish from a call
// that never happened. Prometheus hit the same bug in their own rows and at least kept the row.
//
// So the cost is now recorded whether or not it could be computed, and "could not be computed" is
// carried as nil rather than as 0. The two facts a zero used to merge — nobody priced this, and
// this was priced at zero — are the ones a FinOps dashboard most needs apart.
func (h *ModelGatewayHandlers) recordCost(r *http.Request, result *modelgateway.DecisionResult, runID, agentManifestRef string, response map[string]any) {
	promptTokens, completionTokens := usageTokens(result.Output)

	// OBS-006: an idempotent replay is a real call that consumed NOTHING new. The platform served a
	// stored response and its usage block repeats the ORIGINAL generation's tokens, so recording them
	// again inflates the ledger — upward, which is the direction nobody audits because it looks
	// prudent. Measured: the same body twice under one Idempotency-Key returns usage 12/20 both times,
	// and the replay's own request id has no usage row at all (404).
	//
	// The row is still written, with zeroes and a pointer to the generation that WAS billed. Writing
	// nothing would be worse for the same reason OBS-008 gave: a call that leaves no row cannot be
	// audited, and an audit starting from the replay's id would find nothing on either side. And the
	// zeroes here are a MEASURED zero, not an unknown — no new money was spent, which is why cost_usd
	// is 0 rather than NULL.
	// OBS-005: what actually served this call. The imputation below does NOT change — cost is
	// attributed to the bundle's model, because you pay for the profile and not for the deployment —
	// but both facts are now recorded and the discrepancy is REPORTED rather than left for a caller to
	// notice by comparing two fields.
	//
	// Measured on this deployment: 6 of 6 calls to qwen3-0.6b were served by the same instance
	// (qwen3-0-6b-iq4-nl-local-1) and response.model equalled what was asked, so today there is no
	// discrepancy here. The case that motivates this is Axonium's, against an instance-specific name we
	// have no scope for — so it is THEIR measurement, not one I reproduced, and the divergence path is
	// tested against a controlled provider rather than claimed from the platform.
	//
	// Worth recording what the stability means: because the instance does NOT vary between calls of one
	// profile, this stays a RECORDING problem. If it ever varies, the problem becomes routing and
	// MDL-013 ages with it.
	servedModel := stringField(result.Output, "model")
	servedByInstance := stringField(result.Output, "served_by_instance")
	if servedModel != "" && servedModel != result.Model {
		response["served_model"] = servedModel
		response["served_model_differs"] = true
	}
	if servedByInstance != "" {
		response["served_by_instance"] = servedByInstance
	}

	replayOf := stringField(result.Output, "idempotent_replay_of")
	if replayOf != "" {
		// Explicit pointers to zero, not nil: a replay really did consume nothing new, and that is a
		// MEASURED zero. nil here would say "nobody knows", which would be false — we know exactly.
		zeroTokens := 0
		promptTokens, completionTokens = &zeroTokens, &zeroTokens
		if h.Ledger != nil {
			zero := 0.0
			entry := store.CostEntry{
				Provider: result.ProviderUsed, Model: result.Model,
				CostModel: costModelName(h.Pricing, result.ProviderUsed, result.Model),
				// Explicit zeros, not left nil (MDL-014 + OBS-006): a replay consumed nothing new and we
				// know that exactly. nil here would say "nobody measured", which is false — and it would
				// also make the reconciliation report every replay as tokens_unmeasured.
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				CostUSD:          &zero, RunID: runID, AgentManifestRef: agentManifestRef,
				ProviderRequestID:  stringField(result.Output, "provider_request_id"),
				IdempotentReplayOf: replayOf,
				ServedModel:        servedModel,
				ServedByInstance:   servedByInstance,
			}
			if err := h.Ledger.Record(r.Context(), entry); err != nil {
				log.Printf("aeon-modelgw: recording FinOps replay event: %v", err)
			}
		}
		response["idempotent_replay_of"] = replayOf
		response["cost_usd"] = 0.0
		return
	}

	// Both nil until proven otherwise: no pricing table, no rate, or a cost_model this package
	// cannot price all leave the call recorded and its cost unknown.
	var costModel *string
	var costUSD *float64
	if h.Pricing != nil {
		if rate, ok := h.Pricing.Rate(result.ProviderUsed, result.Model); ok {
			costModel = &rate.CostModel
			response["cost_model"] = rate.CostModel
			// Unpriced when either counter is missing: a price computed from one known half would be a
			// smaller number presented as the whole bill.
			if usd, priced := h.Pricing.CostUSD(result.ProviderUsed, result.Model, tokensOrZero(promptTokens), tokensOrZero(completionTokens)); priced && promptTokens != nil && completionTokens != nil {
				costUSD = &usd
				response["cost_usd"] = usd
			}
		}
	}

	if h.Ledger == nil {
		return
	}
	cacheRead, cacheWrite := usageCacheTokens(result.Output)
	reasoning := optionalInt(usageBlock(result.Output)["reasoning_tokens"])
	entry := store.CostEntry{
		Provider:         result.ProviderUsed,
		Model:            result.Model,
		CostModel:        costModel,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		CostUSD:          costUSD,
		RunID:            runID,
		AgentManifestRef: agentManifestRef,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
		// OBS-007: the provider's id travels in the normalized response because it arrives in a
		// response header and would otherwise be gone by now.
		ProviderRequestID: stringField(result.Output, "provider_request_id"),
		ServedModel:       servedModel,
		ServedByInstance:  servedByInstance,
		ReasoningTokens:   reasoning,
	}
	if err := h.Ledger.Record(r.Context(), entry); err != nil {
		log.Printf("aeon-modelgw: recording FinOps cost event: %v", err)
	}
}

// usageTokens pulls prompt/completion token counts out of a NormalizedChatResponse's usage block,
// tolerating either real Go ints (the in-process case, providers.NormalizedChatResponse's own
// return type) or float64 (were this ever JSON-decoded first) — defensive, not a sign either shape
// is expected in practice.
func usageTokens(output map[string]any) (prompt, completion *int) {
	usage, _ := output["usage"].(map[string]any)
	return optionalInt(usage["prompt_tokens"]), optionalInt(usage["completion_tokens"])
}

// tokensOrZero reads a counter for ARITHMETIC, where an absent one has to become something.
//
// Only for computing a price: cost = tokens x rate, and with no token count there is no price to
// compute — CostUSD then comes back unpriced (OBS-008), which is the honest outcome. This must never be
// used for what gets RECORDED; that is the whole of MDL-014.
func tokensOrZero(v *int) int {
	if v == nil {
		return 0
	}
	return *v
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

// stringField reads an optional string from a normalized response, tolerating its absence — a
// provider that issues no request id omits the key entirely (OBS-007).
func stringField(output map[string]any, key string) string {
	v, _ := output[key].(string)
	return v
}

// costModelName returns the configured cost model for a pair, or nil when none is configured — the
// same three-state rule the priced path uses (OBS-008).
func costModelName(pricing *finops.PricingTable, provider, model string) *string {
	if pricing == nil {
		return nil
	}
	rate, ok := pricing.Rate(provider, model)
	if !ok {
		return nil
	}
	return &rate.CostModel
}
