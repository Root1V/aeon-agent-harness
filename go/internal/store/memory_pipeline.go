package store

import (
	"context"
	"errors"
	"fmt"
)

// memoryValidTransitions maps a memory record's current status to the set of statuses it may move
// to next via the candidate pipeline (MEM-002): quarantine -> validate -> promote, with reject
// (-> REVOKED) available from any pre-ACTIVE stage. Reaching ACTIVE always passes through
// VALIDATED first; skipping a step, moving backward, or requesting the current status is rejected
// (ErrInvalidMemoryTransition). Superseding/revoking an already-ACTIVE record is MEM-005, not here.
var memoryValidTransitions = map[string]map[string]bool{
	MemoryStatusCandidate:   {MemoryStatusQuarantined: true, MemoryStatusRevoked: true},
	MemoryStatusQuarantined: {MemoryStatusValidated: true, MemoryStatusRevoked: true},
	MemoryStatusValidated:   {MemoryStatusActive: true, MemoryStatusRevoked: true},
}

// ErrInvalidMemoryTransition is returned when a requested status change is not one of the current
// status's allowed next steps in the candidate pipeline.
var ErrInvalidMemoryTransition = errors.New("store: invalid memory status transition")

// ErrValidationGateBlocked is returned when a QUARANTINED -> VALIDATED transition is requested
// with a ValidationDecision that did not allow it.
var ErrValidationGateBlocked = errors.New("store: validation gate blocked promotion to VALIDATED")

// ErrPromotionGateBlocked is returned when a VALIDATED -> ACTIVE transition is requested with a
// PromotionDecision that did not allow it (AgentManifest.spec.memoryPolicy.promotionGate:
// eval_required, per the spec's example manifest).
var ErrPromotionGateBlocked = errors.New("store: promotion gate blocked promotion to ACTIVE")

// ValidationDecision is MEM-002's typed verdict on whether a QUARANTINED candidate may move to
// VALIDATED — computed elsewhere (replay/safety/negative-transfer checks; EVAL-004 once it
// exists) and passed in here. Mirrors ReleaseGateDecision's separation of concerns (EVAL-003):
// this store never computes a decision, only applies one.
type ValidationDecision struct {
	Allowed bool
	Reason  string // surfaced in the error when Allowed is false
}

// PromotionDecision is MEM-002's typed verdict on whether a VALIDATED memory may move to ACTIVE —
// the runtime expression of AgentManifest.spec.memoryPolicy.promotionGate: eval_required.
type PromotionDecision struct {
	Allowed bool
	Reason  string // surfaced in the error when Allowed is false
}

// WriteCandidate is the only write path an agent/run ever uses to persist memory
// (AgentManifest.spec.memoryPolicy.writeMode: candidate_only, per the spec's example manifest).
// It always writes status=CANDIDATE, silently overriding whatever status the caller supplied — so
// content an agent (or, upstream of it, untrusted retrieved content reflected back through a
// model) tries to smuggle in as already-ACTIVE/VALIDATED is forced back to CANDIDATE, never
// trusted. This is what keeps "external content never enters active procedural memory directly"
// true at the API, not just as a manifest policy nobody enforces.
func (s *MemoryStore) WriteCandidate(ctx context.Context, rec MemoryRecord) (*MemoryRecord, error) {
	rec.Status = MemoryStatusCandidate
	return s.Create(ctx, rec)
}

// Quarantine moves a CANDIDATE record to QUARANTINED — the pipeline's first curator-driven step,
// flagging a candidate for review before it can ever be validated.
func (s *MemoryStore) Quarantine(ctx context.Context, memoryID string) (*MemoryRecord, error) {
	return s.transitionStatus(ctx, memoryID, MemoryStatusQuarantined)
}

// Validate moves a QUARANTINED record to VALIDATED, but only if decision.Allowed — the gate a
// real replay/safety/negative-transfer check (or, when it exists, EVAL-004) must pass first.
func (s *MemoryStore) Validate(ctx context.Context, memoryID string, decision ValidationDecision) (*MemoryRecord, error) {
	if !decision.Allowed {
		reason := decision.Reason
		if reason == "" {
			reason = "no passing validation decision was provided"
		}
		return nil, fmt.Errorf("%w: %s: %s", ErrValidationGateBlocked, memoryID, reason)
	}
	return s.transitionStatus(ctx, memoryID, MemoryStatusValidated)
}

// Promote moves a VALIDATED record to ACTIVE, but only if decision.Allowed — this is the only path
// by which a memory record ever becomes ACTIVE (Create/WriteCandidate can never do this directly,
// see MEM-001's ErrDirectActiveWriteRejected).
func (s *MemoryStore) Promote(ctx context.Context, memoryID string, decision PromotionDecision) (*MemoryRecord, error) {
	if !decision.Allowed {
		reason := decision.Reason
		if reason == "" {
			reason = "no passing promotion decision was provided"
		}
		return nil, fmt.Errorf("%w: %s: %s", ErrPromotionGateBlocked, memoryID, reason)
	}
	return s.transitionStatus(ctx, memoryID, MemoryStatusActive)
}

// Reject moves a CANDIDATE, QUARANTINED, or VALIDATED record to REVOKED — the pipeline's terminal
// "reject" outcome. An already-ACTIVE record is out of scope here: superseding/revoking a promoted
// memory is MEM-005 (Utility/Forgetting), not this pipeline.
func (s *MemoryStore) Reject(ctx context.Context, memoryID string) (*MemoryRecord, error) {
	return s.transitionStatus(ctx, memoryID, MemoryStatusRevoked)
}

func (s *MemoryStore) transitionStatus(ctx context.Context, memoryID, target string) (*MemoryRecord, error) {
	current, err := s.Get(ctx, memoryID)
	if err != nil {
		return nil, err
	}
	allowed, ok := memoryValidTransitions[current.Status]
	if !ok || !allowed[target] {
		return nil, fmt.Errorf("%w: %s is %s, cannot move to %s", ErrInvalidMemoryTransition, memoryID, current.Status, target)
	}

	_, err = s.pool.Exec(ctx,
		`UPDATE memory_records SET status = $1, updated_at = now() WHERE memory_id = $2`,
		target, memoryID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: transition memory status: %w", err)
	}
	return s.Get(ctx, memoryID)
}
