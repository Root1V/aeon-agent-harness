package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/aeon-ai/aeon/go/internal/store"
)

// MemoryHandlers exposes the Memory Store + Candidate Pipeline (MEM-001/MEM-002) over HTTP — the
// surface a Python run (Reflection, MEM-003) uses to write/read/promote memory, since aeon_worker
// has no direct Postgres access of its own (the same separation the Agent/Tool Registry already
// use). writeCandidate is deliberately the only write route a caller not operating the pipeline
// itself needs: it always forces status=CANDIDATE (writeMode: candidate_only), same guarantee as
// MemoryStore.WriteCandidate's own docstring.
type MemoryHandlers struct {
	MemoryStore *store.MemoryStore
}

// Register mounts every memory route on mux.
func (h *MemoryHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /memory/candidates", h.writeCandidate)
	mux.HandleFunc("GET /memory/active", h.listActive)
	mux.HandleFunc("GET /memory/{memory_id}", h.getMemory)
	mux.HandleFunc("POST /memory/{memory_id}/quarantine", h.quarantine)
	mux.HandleFunc("POST /memory/{memory_id}/validate", h.validate)
	mux.HandleFunc("POST /memory/{memory_id}/promote", h.promote)
	mux.HandleFunc("POST /memory/{memory_id}/reject", h.reject)
}

func (h *MemoryHandlers) writeCandidate(w http.ResponseWriter, r *http.Request) {
	var rec store.MemoryRecord
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	created, err := h.MemoryStore.WriteCandidate(r.Context(), rec)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// listActive is the governed read path: GET /memory/active?scope=project,tenant&tenant_id=acme —
// mirrors AgentManifest.spec.memoryPolicy.readScopes (a run only ever asks for the scopes its own
// manifest allows; this handler doesn't decide that, it just requires the caller to name them).
func (h *MemoryHandlers) listActive(w http.ResponseWriter, r *http.Request) {
	var scopes []string
	for _, s := range strings.Split(r.URL.Query().Get("scope"), ",") {
		if s != "" {
			scopes = append(scopes, s)
		}
	}
	if len(scopes) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("api: at least one 'scope' query value is required"))
		return
	}
	recs, err := h.MemoryStore.ListActive(r.Context(), scopes, r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

func (h *MemoryHandlers) getMemory(w http.ResponseWriter, r *http.Request) {
	rec, err := h.MemoryStore.Get(r.Context(), r.PathValue("memory_id"))
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *MemoryHandlers) quarantine(w http.ResponseWriter, r *http.Request) {
	rec, err := h.MemoryStore.Quarantine(r.Context(), r.PathValue("memory_id"))
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *MemoryHandlers) validate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := h.MemoryStore.Validate(r.Context(), r.PathValue("memory_id"), store.ValidationDecision{Allowed: body.Allowed, Reason: body.Reason})
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *MemoryHandlers) promote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := h.MemoryStore.Promote(r.Context(), r.PathValue("memory_id"), store.PromotionDecision{Allowed: body.Allowed, Reason: body.Reason})
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *MemoryHandlers) reject(w http.ResponseWriter, r *http.Request) {
	rec, err := h.MemoryStore.Reject(r.Context(), r.PathValue("memory_id"))
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func writeMemoryStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrAlreadyExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, store.ErrInvalidMemoryTransition),
		errors.Is(err, store.ErrValidationGateBlocked),
		errors.Is(err, store.ErrPromotionGateBlocked),
		errors.Is(err, store.ErrDirectActiveWriteRejected),
		errors.Is(err, store.ErrInvalidMemoryRecord):
		writeError(w, http.StatusUnprocessableEntity, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}
