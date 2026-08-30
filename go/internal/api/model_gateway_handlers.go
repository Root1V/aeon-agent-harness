package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
)

// ModelGatewayHandlers exposes the Model Gateway's routing/fallback (MDL-001) over HTTP — the only
// network-reachable way anything outside this process (the Python worker, starting with DR-001's
// Research Planner) can ask a provider to decide. Per docs/adr/0004: no caller other than this
// gateway ever talks to a provider SDK directly.
type ModelGatewayHandlers struct {
	Gateway *modelgateway.Gateway
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
	writeJSON(w, http.StatusOK, map[string]any{
		"provider_used": result.ProviderUsed,
		"model":         result.Model,
		"output":        result.Output,
		"attempts":      result.Attempts,
	})
}
