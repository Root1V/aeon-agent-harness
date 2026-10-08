package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/aeon-ai/aeon/go/internal/auth"
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
	mux.HandleFunc("POST /memory/{memory_id}/revoke", h.revoke)
	mux.HandleFunc("POST /memory/{memory_id}/repair", h.repair)
}

// callerTenant resolves the isolation boundary for this request from the CREDENTIAL (VRT-AEON-005
// T-1), never from the request.
//
// WHAT THIS REPLACED, measured by reading the handlers below as they were: `listActive`, `getMemory`
// and `revoke` took `tenant_id` from a query value or the body, so the isolation SEC-004 built and
// tested was real and the tenant was whatever the caller typed. `TestMemoryHandlersGetIsIsolatedByTenant`
// proved that naming the WRONG tenant returns 404 — not that a caller cannot name another tenant.
// And `quarantine`, `validate`, `promote`, `reject` and `repair` checked no tenant at all, so any
// authenticated caller could quarantine or promote another tenant's memory knowing only its id.
//
// It is the same defect SEC-005 removed from the Cedar principal, one field name along: an engine
// that is default-deny, well tested, and judging an identity the caller supplied.
func callerTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	caller, ok := auth.CallerFrom(r.Context())
	if !ok {
		// No caller means this route was mounted without auth.Require. Serving it anyway is how an
		// unauthenticated surface comes back one wiring mistake at a time.
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthenticated: this route was reached without an authenticated caller (SEC-005)",
		})
		return "", false
	}
	return caller.Tenant, true
}

// rejectRequestTenant refuses a request that carries a tenant of its own.
//
// IGNORING IT WOULD BE WORSE THAN REFUSING. A client that sends `tenant_id` believes it chose the
// boundary; if the value agrees it is noise, and if it disagrees the client is wrong about something
// that matters and will go on being wrong. The refusal says where the tenant comes from instead.
func rejectRequestTenant(w http.ResponseWriter, supplied, actual string) bool {
	if supplied == "" || supplied == actual {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": "this request names a tenant_id, and the tenant is taken from the caller's credential " +
			"rather than from the request (VRT-AEON-005 T-1). Remove the field",
		"caller_tenant": actual,
	})
	return true
}

// ownedByCaller loads memoryID and hands it back only if it belongs to the caller's tenant.
//
// CROSS-TENANT IS INDISTINGUISHABLE FROM NONEXISTENT (T-7), and 403 would be the wrong answer
// however tempting: a 403 confirms the record exists. Within a tenant the 403s elsewhere in this
// codebase stay — "you may not act as this agent" and "no such agent" are different facts, and only
// the tenant boundary has to hide which one it is.
func (h *MemoryHandlers) ownedByCaller(w http.ResponseWriter, r *http.Request, tenant string) (*store.MemoryRecord, bool) {
	rec, err := h.MemoryStore.Get(r.Context(), r.PathValue("memory_id"))
	if err != nil {
		writeMemoryStoreError(w, err)
		return nil, false
	}
	if rec.TenantID != tenant {
		writeMemoryStoreError(w, store.ErrNotFound)
		return nil, false
	}
	return rec, true
}

func (h *MemoryHandlers) writeCandidate(w http.ResponseWriter, r *http.Request) {
	tenant, ok := callerTenant(w, r)
	if !ok {
		return
	}
	var rec store.MemoryRecord
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if rejectRequestTenant(w, rec.TenantID, tenant) {
		return
	}
	// Set, not merely checked: a write whose tenant came from the body let a caller put a record
	// into another tenant's store, which is the worst of the routes here — reading another tenant's
	// memory is a disclosure, writing into it is an injection into somebody else's agent context.
	rec.TenantID = tenant
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
	tenant, ok := callerTenant(w, r)
	if !ok {
		return
	}
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
	if rejectRequestTenant(w, r.URL.Query().Get("tenant_id"), tenant) {
		return
	}
	recs, err := h.MemoryStore.ListActive(r.Context(), scopes, tenant)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

// getMemory requires a tenant_id query value and returns 404 — not the record — when it doesn't
// match the record's own tenant_id (SEC-004 isolation): knowing a memory_id (a UUID, but not a
// secret) must never be enough to read another tenant's memory content, and a wrong tenant_id
// must look identical to "doesn't exist" rather than confirming the record is real.
func (h *MemoryHandlers) getMemory(w http.ResponseWriter, r *http.Request) {
	tenant, ok := callerTenant(w, r)
	if !ok {
		return
	}
	if rejectRequestTenant(w, r.URL.Query().Get("tenant_id"), tenant) {
		return
	}
	rec, ok := h.ownedByCaller(w, r, tenant)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// revoke requires the caller to name the tenant_id it believes owns memoryID, and refuses (as a
// 404, same isolation reasoning as getMemory) if it doesn't match — a cross-tenant caller cannot
// revoke, or even confirm the existence of, another tenant's memory.
func (h *MemoryHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	tenant, ok := callerTenant(w, r)
	if !ok {
		return
	}
	// The body is still decoded, and a tenant in it is still refused rather than ignored — a caller
	// that names one believes it is choosing.
	var body struct {
		TenantID string `json:"tenant_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if rejectRequestTenant(w, body.TenantID, tenant) {
		return
	}
	if _, ok := h.ownedByCaller(w, r, tenant); !ok {
		return
	}
	rec, err := h.MemoryStore.Revoke(r.Context(), r.PathValue("memory_id"))
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// repair runs SEC-004's tamper check/response (MemoryStore.RepairIfTampered) and reports whether
// a repair (revocation) happened.
func (h *MemoryHandlers) repair(w http.ResponseWriter, r *http.Request) {
	// VRT-AEON-005 T-1: this route had NO tenant check at all before 2026-10-07, so any
	// authenticated caller could act on another tenant's memory knowing only its id — a uuid, which
	// is not a secret. SEC-004's isolation covered get/list/revoke and these five were missed, which
	// is what an isolation built route by route rather than at a seam looks like after a while.
	tenant, ok := callerTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.ownedByCaller(w, r, tenant); !ok {
		return
	}
	tampered, rec, err := h.MemoryStore.RepairIfTampered(r.Context(), r.PathValue("memory_id"))
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tampered": tampered, "memory": rec})
}

func (h *MemoryHandlers) quarantine(w http.ResponseWriter, r *http.Request) {
	// VRT-AEON-005 T-1: this route had NO tenant check at all before 2026-10-07, so any
	// authenticated caller could act on another tenant's memory knowing only its id — a uuid, which
	// is not a secret. SEC-004's isolation covered get/list/revoke and these five were missed, which
	// is what an isolation built route by route rather than at a seam looks like after a while.
	tenant, ok := callerTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.ownedByCaller(w, r, tenant); !ok {
		return
	}
	rec, err := h.MemoryStore.Quarantine(r.Context(), r.PathValue("memory_id"))
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *MemoryHandlers) validate(w http.ResponseWriter, r *http.Request) {
	// VRT-AEON-005 T-1: this route had NO tenant check at all before 2026-10-07, so any
	// authenticated caller could act on another tenant's memory knowing only its id — a uuid, which
	// is not a secret. SEC-004's isolation covered get/list/revoke and these five were missed, which
	// is what an isolation built route by route rather than at a seam looks like after a while.
	tenant, ok := callerTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.ownedByCaller(w, r, tenant); !ok {
		return
	}
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
	// VRT-AEON-005 T-1: this route had NO tenant check at all before 2026-10-07, so any
	// authenticated caller could act on another tenant's memory knowing only its id — a uuid, which
	// is not a secret. SEC-004's isolation covered get/list/revoke and these five were missed, which
	// is what an isolation built route by route rather than at a seam looks like after a while.
	tenant, ok := callerTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.ownedByCaller(w, r, tenant); !ok {
		return
	}
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
	// VRT-AEON-005 T-1: this route had NO tenant check at all before 2026-10-07, so any
	// authenticated caller could act on another tenant's memory knowing only its id — a uuid, which
	// is not a secret. SEC-004's isolation covered get/list/revoke and these five were missed, which
	// is what an isolation built route by route rather than at a seam looks like after a while.
	tenant, ok := callerTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.ownedByCaller(w, r, tenant); !ok {
		return
	}
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
