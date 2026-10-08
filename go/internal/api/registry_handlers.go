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

	"github.com/aeon-ai/aeon/go/internal/auth"
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

// registryFor resolves the tenant from the CREDENTIAL and returns that tenant's registries
// (VRT-AEON-005 T-4). Before migration 0002 the agent key was (name, version) and the tool key
// (tool_id, version), so two tenants could not use the same manifest name and either could list the
// other's. Both are now scoped, and the handle cannot be built without a tenant.
func (h *RegistryHandlers) tenantFor(w http.ResponseWriter, r *http.Request) (string, bool) {
	caller, ok := auth.CallerFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthenticated: the registries are scoped to the caller's tenant (SEC-005/VRT-AEON-005)",
		})
		return "", false
	}
	return caller.Tenant, true
}

func (h *RegistryHandlers) createAgent(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Manifest map[string]any `json:"manifest"`
		Owner    string         `json:"owner"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := h.Store.AgentRegistryFor(tenant).Create(r.Context(), body.Manifest, body.Owner)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (h *RegistryHandlers) listAgents(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantFor(w, r)
	if !ok {
		return
	}
	recs, err := h.Store.AgentRegistryFor(tenant).List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

func (h *RegistryHandlers) getAgent(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantFor(w, r)
	if !ok {
		return
	}
	rec, err := h.Store.AgentRegistryFor(tenant).Get(r.Context(), r.PathValue("name"), r.PathValue("version"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *RegistryHandlers) transitionAgent(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Target string `json:"target"`
		// ReleaseGate is EVAL-003's verdict — computed elsewhere (aeon_evalops.release_gate) and
		// passed through here. Only checked for the Candidate -> Released step; omit it (or set
		// allowed:false) for every other transition, which ignores it entirely.
		ReleaseGate struct {
			Allowed bool   `json:"allowed"`
			Reason  string `json:"reason"`
		} `json:"release_gate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	gate := store.ReleaseGateDecision{Allowed: body.ReleaseGate.Allowed, Reason: body.ReleaseGate.Reason}
	rec, err := h.Store.AgentRegistryFor(tenant).TransitionLifecycle(r.Context(), r.PathValue("name"), r.PathValue("version"), body.Target, gate)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *RegistryHandlers) createTool(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantFor(w, r)
	if !ok {
		return
	}
	var descriptor map[string]any
	if err := json.NewDecoder(r.Body).Decode(&descriptor); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := h.Store.ToolRegistryFor(tenant).Create(r.Context(), descriptor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (h *RegistryHandlers) listTools(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantFor(w, r)
	if !ok {
		return
	}
	recs, err := h.Store.ToolRegistryFor(tenant).List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

func (h *RegistryHandlers) getTool(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantFor(w, r)
	if !ok {
		return
	}
	rec, err := h.Store.ToolRegistryFor(tenant).Get(r.Context(), r.PathValue("tool_id"), r.PathValue("version"))
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
	case errors.Is(err, store.ErrInvalidTransition), errors.Is(err, store.ErrReleaseGateBlocked):
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
