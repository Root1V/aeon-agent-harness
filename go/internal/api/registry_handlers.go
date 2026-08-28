// Package api wires the control plane's registries (go/internal/store) to HTTP. Kept deliberately
// small and dependency-free (net/http only) — this is FND-001/TOOL-001's CRUD surface, not a
// framework choice; gRPC (proto/aeon/v1) is the contract for gateway-to-gateway calls, this REST
// surface is for the CLI/UI/operators.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/aeon-ai/aeon/go/internal/store"
)

// RegistryHandlers holds the dependencies the HTTP handlers need.
type RegistryHandlers struct {
	Store *store.Store
}

// Register mounts every registry route on mux.
func (h *RegistryHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /agents", h.createAgent)
	mux.HandleFunc("GET /agents", h.listAgents)
	mux.HandleFunc("GET /agents/{name}/{version}", h.getAgent)
	mux.HandleFunc("POST /agents/{name}/{version}/transition", h.transitionAgent)

	mux.HandleFunc("POST /tools", h.createTool)
	mux.HandleFunc("GET /tools", h.listTools)
	mux.HandleFunc("GET /tools/{tool_id}/{version}", h.getTool)
}

func (h *RegistryHandlers) createAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Manifest map[string]any `json:"manifest"`
		Owner    string         `json:"owner"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := h.Store.AgentRegistry().Create(r.Context(), body.Manifest, body.Owner)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (h *RegistryHandlers) listAgents(w http.ResponseWriter, r *http.Request) {
	recs, err := h.Store.AgentRegistry().List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

func (h *RegistryHandlers) getAgent(w http.ResponseWriter, r *http.Request) {
	rec, err := h.Store.AgentRegistry().Get(r.Context(), r.PathValue("name"), r.PathValue("version"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *RegistryHandlers) transitionAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := h.Store.AgentRegistry().TransitionLifecycle(r.Context(), r.PathValue("name"), r.PathValue("version"), body.Target)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *RegistryHandlers) createTool(w http.ResponseWriter, r *http.Request) {
	var descriptor map[string]any
	if err := json.NewDecoder(r.Body).Decode(&descriptor); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := h.Store.ToolRegistry().Create(r.Context(), descriptor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (h *RegistryHandlers) listTools(w http.ResponseWriter, r *http.Request) {
	recs, err := h.Store.ToolRegistry().List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

func (h *RegistryHandlers) getTool(w http.ResponseWriter, r *http.Request) {
	rec, err := h.Store.ToolRegistry().Get(r.Context(), r.PathValue("tool_id"), r.PathValue("version"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrAlreadyExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, store.ErrInvalidTransition):
		writeError(w, http.StatusUnprocessableEntity, err)
	case strings.Contains(err.Error(), "idempotency_key_fields"),
		strings.Contains(err.Error(), "invalid side_effect"),
		strings.Contains(err.Error(), "invalid risk"),
		strings.Contains(err.Error(), "required"):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
