package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
)

// OpenAICompatibleHandlers exposes the Model Gateway as a real OpenAI Chat Completions endpoint
// (INT-002, Modo C — roadmap.md §2.5): any framework that already speaks OpenAI's wire format gets
// Aeon's routing/fallback by changing only its base_url, no code change. "model" in the request is
// interpreted as a capability profile name (docs/adr/0004) — never a concrete provider/model —
// resolved against a real, config-as-code ModelPolicyBundle (FND-003), the same file
// aeon_sdk.model_policy.resolve_candidates resolves on the Python side.
//
// VRT-AEON-003 A-3 closed the gap this comment used to declare ("budget/cost enforcement at this
// endpoint is not implemented yet"). It was an honest note and it was also the whole problem: a
// framework pointed at this base_url got Aeon's ROUTING with none of Aeon's GOVERNANCE, so the same
// deployment had a governed door and an ungoverned one, and the ungoverned one was the one marketed
// as needing no code change. Veritium reported it.
//
// Governance is not reimplemented here. It is the SAME ModelGatewayHandlers /decide uses, called
// through the same two methods, because two implementations of a ceiling is how one of them ends up
// three months behind — which is exactly the state this endpoint was found in.
type OpenAICompatibleHandlers struct {
	Gateway *modelgateway.Gateway
	Bundle  modelgateway.ModelPolicyBundleDoc
	// Governance holds the cost ceiling (MDL-017) and the FinOps ledger write (OBS-003/008). Optional
	// and nil-safe, like the ledger it wraps — but unlike before, a call that is NOT governed says so
	// in its own response rather than leaving the caller to assume it was.
	Governance *ModelGatewayHandlers
}

// The governance headers. Headers and not body fields on purpose: this endpoint's contract is
// someone else's (OpenAI's), and adding required properties to a foreign request schema is how a
// "no code change" integration stops being one. A client that knows nothing about Aeon sends none of
// these and still works.
const (
	// RunIDHeader attributes the call to a run, which is what makes a ceiling and a per-run cost
	// possible at all — there is nothing to sum without it.
	RunIDHeader = "X-Aeon-Run-Id"
	// AgentManifestRefHeader names the agent whose manifest DECLARES the ceiling.
	AgentManifestRefHeader = "X-Aeon-Agent-Manifest-Ref"
	// IdempotencyKeyHeader is the spelling OpenAI's own clients already send, so a caller that
	// retries safely against OpenAI retries safely here with no change.
	IdempotencyKeyHeader = "Idempotency-Key"
)

// governanceContext reads the three headers and folds the idempotency key into the request body,
// which is where the provider layer looks for it.
//
// The key travels in the BODY and not in a header from here on because that is already the contract
// /decide callers use (rendered_context.idempotency_key) and the one the provider client reads. The
// const is imported rather than retyped: a literal "idempotency_key" here would keep working on the
// day that field is renamed, and keep working is the failure — the call would silently stop being
// idempotent and the only symptom would be a ledger that charges twice.
func governanceContext(r *http.Request, body map[string]any) (runID, agentRef, idempotencyKey string) {
	runID = strings.TrimSpace(r.Header.Get(RunIDHeader))
	agentRef = strings.TrimSpace(r.Header.Get(AgentManifestRefHeader))
	idempotencyKey = strings.TrimSpace(r.Header.Get(IdempotencyKeyHeader))
	if idempotencyKey != "" {
		if _, alreadySet := body[prometheusinference.IdempotencyKeyField]; !alreadySet {
			body[prometheusinference.IdempotencyKeyField] = idempotencyKey
		}
	}
	return runID, agentRef, idempotencyKey
}

// governanceReport is what "absence of the headers keeps working but is reported" means concretely:
// three states and not two — governed, ungoverned because this deployment has no ledger, ungoverned
// because this CALL named no run or no agent. A caller that cannot tell them apart has to guess
// whether its budget is being enforced, and the usual guess is yes.
func (h *OpenAICompatibleHandlers) governanceReport(runID, agentRef, idempotencyKey string) map[string]any {
	report := map[string]any{}
	if runID != "" {
		report["run_id"] = runID
	}
	if agentRef != "" {
		report["agent_manifest_ref"] = agentRef
	}
	if idempotencyKey != "" {
		report["idempotency_key"] = idempotencyKey
	}

	var reasons []string
	if h.Governance == nil || h.Governance.Ledger == nil || h.Governance.Agents == nil {
		reasons = append(reasons, "this gateway has no cost ledger or agent registry configured, so no "+
			"ceiling is enforced and no cost is recorded (needs AEON_PG_DSN)")
	} else {
		if runID == "" {
			reasons = append(reasons, "no "+RunIDHeader+" header: this call is not attributed to a run, so "+
				"its cost is recorded without one and no per-run ceiling can apply to it")
		}
		if agentRef == "" {
			reasons = append(reasons, "no "+AgentManifestRefHeader+" header: the ceiling is declared in an "+
				"agent manifest, and without the ref there is no manifest to read it from")
		}
	}
	if idempotencyKey == "" {
		// Not an ungoverned reason — a call that is not retried needs no key. Reported because a
		// caller that BELIEVES it sent one deserves to find out here rather than from the bill.
		report["idempotent"] = false
	} else {
		report["idempotent"] = true
	}

	report["governed"] = len(reasons) == 0
	if len(reasons) > 0 {
		report["ungoverned_reasons"] = reasons
	}
	return report
}

// Register mounts the OpenAI-compatible routes on mux.
func (h *OpenAICompatibleHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", h.chatCompletions)
}

func (h *OpenAICompatibleHandlers) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	profile, _ := body["model"].(string)
	if profile == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "'model' (a capability profile name) is required")
		return
	}

	candidates, dataSensitivity, err := h.Bundle.ResolveProfile(profile)
	if err != nil {
		writeOpenAIError(w, resolveErrorStatus(err), resolveErrorType(err), err.Error())
		return
	}

	runID, agentRef, idempotencyKey := governanceContext(r, body)
	governance := h.governanceReport(runID, agentRef, idempotencyKey)

	// The ceiling, checked BEFORE the provider is called — the same ordering and the same reason as
	// /decide: checking afterwards records the spend and then refuses, which is a receipt and not a
	// cap. Same method, so there is no second copy of the rule to fall behind.
	if h.Governance != nil {
		if refusal := h.Governance.overBudget(r, runID, agentRef); refusal != nil {
			// 402 in OpenAI's error envelope, so a client that only understands that envelope still
			// reads a message; the facts a budget stop carries (ceiling, spend, retryable: false) ride
			// alongside under `aeon` rather than being flattened into the message string.
			message, _ := refusal["error"].(string)
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"error": map[string]any{"message": message, "type": "aeon_budget_exceeded"},
				"aeon":  refusal,
			})
			return
		}
	}

	if stream, _ := body["stream"].(bool); stream {
		h.streamChatCompletions(w, r, candidates, body, dataSensitivity, runID, agentRef, governance)
		return
	}

	result, err := h.Gateway.Decide(r.Context(), candidates, body, dataSensitivity)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "aeon_routing_error", err.Error())
		return
	}

	// VRT-SYN-004: under `aeon` and not at the response root, for the same reason the cost fields
	// are — a client parsing a ChatCompletion must not meet unknown top-level keys. Absent when
	// nothing was dropped: absence says the request went out as the caller built it.
	if result.UnsendableTurnsDropped > 0 {
		governance["unsendable_turns_dropped"] = result.UnsendableTurnsDropped
	}

	response := toOpenAIChatCompletionResponse(result.Output)
	if h.Governance != nil {
		// The ledger write, OBS-003/006/008 and all. Writing into `governance` rather than into the
		// response root keeps OpenAI's schema untouched: cost_usd, cost_model and idempotent_replay_of
		// are Aeon's vocabulary, and a client parsing a ChatCompletion must not meet unknown
		// top-level keys it may validate strictly.
		h.Governance.recordCost(r, result, runID, agentRef, governance)
	}
	response["aeon"] = governance

	writeJSON(w, http.StatusOK, response)
}

// toOpenAIChatCompletionResponse adds the envelope fields a real OpenAI Chat Completions response
// needs (id/object/created) around providers.NormalizedChatResponse's output — which is already
// OpenAI-shaped by design (model/choices/usage), so nothing else needs translating.
func toOpenAIChatCompletionResponse(normalized map[string]any) map[string]any {
	response := map[string]any{
		"id":      "chatcmpl-" + uuid.NewString(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
	}
	for k, v := range normalized {
		response[k] = v
	}
	return response
}

func writeOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": errType},
	})
}

// resolveErrorStatus and resolveErrorType keep a policy denial distinguishable from "you asked for a
// profile that does not exist" (MDL-011). Both used to be a 404 invalid_request_error, which is the
// shape of a client typo — and a caller cannot act on a denial it cannot tell apart from a typo.
func isPolicyDenial(err error) bool {
	return errors.Is(err, modelgateway.ErrCandidateModalityMismatch) ||
		errors.Is(err, modelgateway.ErrInferenceClassUndeclared) ||
		errors.Is(err, modelgateway.ErrLocalInferenceProviderDenied)
}

func resolveErrorStatus(err error) int {
	if isPolicyDenial(err) {
		return http.StatusForbidden
	}
	return http.StatusNotFound
}

func resolveErrorType(err error) string {
	if isPolicyDenial(err) {
		return "aeon_policy_denied"
	}
	return "invalid_request_error"
}
