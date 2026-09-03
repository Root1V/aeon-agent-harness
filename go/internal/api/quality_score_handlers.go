package api

import (
	"encoding/json"
	"net/http"

	"github.com/aeon-ai/aeon/go/internal/store"
)

// QualityScoreHandlers exposes MDL-002's real write path: an eval suite (provider_conformance, or
// any future one) reports the score it just computed for a (provider, model) pair, which
// modelgateway.Gateway.Quality (a *store.QualityScoreStore) then consults on every future /decide
// call. No real caller does this automatically yet — see backlog.md — this is the real, tested
// surface that wiring will call into.
type QualityScoreHandlers struct {
	Scores *store.QualityScoreStore
}

// Register mounts the quality score routes on mux.
func (h *QualityScoreHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /quality-scores", h.report)
	mux.HandleFunc("GET /quality-scores", h.list)
}

type reportQualityScoreRequest struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Suite    string  `json:"suite"`
	Score    float64 `json:"score"`
}

func (h *QualityScoreHandlers) report(w http.ResponseWriter, r *http.Request) {
	var body reportQualityScoreRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Provider == "" || body.Model == "" || body.Suite == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider, model, and suite are required"})
		return
	}
	if err := h.Scores.Report(r.Context(), body.Provider, body.Model, body.Suite, body.Score); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": body.Provider,
		"model":    body.Model,
		"suite":    body.Suite,
		"score":    body.Score,
	})
}

func (h *QualityScoreHandlers) list(w http.ResponseWriter, r *http.Request) {
	scores, err := h.Scores.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scores": scores})
}
