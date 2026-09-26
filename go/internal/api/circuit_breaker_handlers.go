package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/aeon-ai/aeon/go/internal/circuitbreaker"
	"github.com/aeon-ai/aeon/go/internal/secrets"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// CircuitBreakerHandlers exposes A5's circuit breaker + kill switch as real HTTP: reporting a run's
// outcome can automatically trip a version into quarantine, and an operator can trip or clear one
// manually at any time. Secrets is optional — when set, tripping a version also revokes every
// secret lease tagged with that agent's identity (secrets.Broker.RevokeAllForOwner), the "hot
// credential revocation" half of A5. Nothing yet calls POST /outcomes automatically when a real run
// finishes (see backlog.md) — this is the real breaker/enforcement surface, wiring a live run's
// completion to report here is separate integration work.
type CircuitBreakerHandlers struct {
	Registry *store.AgentRegistry
	Breaker  *circuitbreaker.Breaker
	Secrets  *secrets.Broker // optional; nil disables credential revocation on trip
}

// Register mounts the circuit breaker routes on mux.
func (h *CircuitBreakerHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /agents/{name}/{version}/outcomes", h.recordOutcome)
	mux.HandleFunc("POST /agents/{name}/{version}/quarantine", h.quarantine)
	mux.HandleFunc("POST /agents/{name}/{version}/unquarantine", h.unquarantine)
}

type recordOutcomeRequest struct {
	Success bool `json:"success"`
	// CostUSD is a POINTER, and that is OBS-009's fix rather than a style choice. As a float64 with
	// omitempty, a caller that omitted the field — a compute_based run, a model with no configured
	// rate — was indistinguishable from one reporting $0, and the breaker averaged those zeros into
	// its cost threshold. The more unpriced spend a version had, the safer it looked. A pointer makes
	// "not reported" arrive as nil, and the breaker then averages only what was measured.
	CostUSD *float64 `json:"cost_usd"`
}

func (h *CircuitBreakerHandlers) recordOutcome(w http.ResponseWriter, r *http.Request) {
	name, version := r.PathValue("name"), r.PathValue("version")
	var body recordOutcomeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	verdict := h.Breaker.RecordOutcome(name, version, circuitbreaker.Observation{Success: body.Success, CostUSD: body.CostUSD})
	if !verdict.Tripped {
		writeJSON(w, http.StatusOK, map[string]any{"tripped": false})
		return
	}

	rec, err := h.applyQuarantine(r.Context(), name, version, verdict.Reason)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("circuit breaker tripped but applying quarantine failed: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tripped": true, "reason": verdict.Reason, "agent": rec})
}

type quarantineRequest struct {
	Reason string `json:"reason"`
}

// quarantine is the manual kill switch: immediate, no threshold check, any reason an operator
// gives. Real, not a formality — it's the exact same store.AgentRegistry.Quarantine call an
// automatic trip makes.
func (h *CircuitBreakerHandlers) quarantine(w http.ResponseWriter, r *http.Request) {
	name, version := r.PathValue("name"), r.PathValue("version")
	var body quarantineRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	reason := body.Reason
	if reason == "" {
		reason = "manual kill switch"
	}

	rec, err := h.applyQuarantine(r.Context(), name, version, reason)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, store.ErrNotReleased) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *CircuitBreakerHandlers) unquarantine(w http.ResponseWriter, r *http.Request) {
	name, version := r.PathValue("name"), r.PathValue("version")
	rec, err := h.Registry.Unquarantine(r.Context(), name, version)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	h.Breaker.Reset(name, version)
	writeJSON(w, http.StatusOK, rec)
}

// applyQuarantine is the one place that actually trips a version: it updates the durable registry
// record and, if a Broker is configured, revokes every secret lease tagged with this agent's
// identity — the real "hot credential revocation" A5 promises. The registry update is the
// authoritative, durable part; the credential revocation follows it best-effort (a Secrets-less
// deployment still gets a correct quarantine, just without the revocation side effect).
func (h *CircuitBreakerHandlers) applyQuarantine(ctx context.Context, name, version, reason string) (*store.AgentRecord, error) {
	rec, err := h.Registry.Quarantine(ctx, name, version, reason)
	if err != nil {
		return nil, err
	}
	if h.Secrets != nil {
		h.Secrets.RevokeAllForOwner(name + "@" + version)
	}
	return rec, nil
}
