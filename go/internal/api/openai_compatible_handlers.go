package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
)

// OpenAICompatibleHandlers exposes the Model Gateway as a real OpenAI Chat Completions endpoint
// (INT-002, Modo C — roadmap.md §2.5): any framework that already speaks OpenAI's wire format gets
// Aeon's routing/fallback by changing only its base_url, no code change. "model" in the request is
// interpreted as a capability profile name (docs/adr/0004) — never a concrete provider/model —
// resolved against a real, config-as-code ModelPolicyBundle (FND-003), the same file
// aeon_sdk.model_policy.resolve_candidates resolves on the Python side.
//
// Budget/cost enforcement at this endpoint is not implemented yet (see roadmap.md/backlog.md) —
// this is the real routing/fallback path only.
type OpenAICompatibleHandlers struct {
	Gateway *modelgateway.Gateway
	Bundle  modelgateway.ModelPolicyBundleDoc
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

	if stream, _ := body["stream"].(bool); stream {
		h.streamChatCompletions(w, r, candidates, body, dataSensitivity)
		return
	}

	result, err := h.Gateway.Decide(r.Context(), candidates, body, dataSensitivity)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "aeon_routing_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toOpenAIChatCompletionResponse(result.Output))
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
func resolveErrorStatus(err error) int {
	if errors.Is(err, modelgateway.ErrCandidateModalityMismatch) {
		return http.StatusForbidden
	}
	return http.StatusNotFound
}

func resolveErrorType(err error) string {
	if errors.Is(err, modelgateway.ErrCandidateModalityMismatch) {
		return "aeon_policy_denied"
	}
	return "invalid_request_error"
}
