package store

import (
	"context"
	"errors"
	"testing"
)

// TestMemoryWriteModeCandidateOnly is MEM-002's acceptance test: AgentManifest.spec.memoryPolicy's
// writeMode: candidate_only is enforced at the API, not merely documented. Even a caller that
// tries to smuggle in an already-ACTIVE (or VALIDATED, QUARANTINED, REVOKED, ...) record through
// the agent-facing write path gets back a CANDIDATE — WriteCandidate never trusts a caller's
// status field.
func TestMemoryWriteModeCandidateOnly(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()

	for _, attemptedStatus := range []string{
		MemoryStatusCandidate, MemoryStatusQuarantined, MemoryStatusValidated,
		MemoryStatusActive, MemoryStatusSuperseded, MemoryStatusRevoked,
	} {
		rec, err := ms.WriteCandidate(ctx, MemoryRecord{
			Type: MemoryTypeSemantic, Scope: MemoryScopeSession,
			Content: "an agent's own write attempt", Status: attemptedStatus,
		})
		if err != nil {
			t.Fatalf("WriteCandidate (attempted status %q): %v", attemptedStatus, err)
		}
		if rec.Status != MemoryStatusCandidate {
			t.Errorf("attempted status %q: got status %q, want %q — write_mode: candidate_only must be enforced regardless of caller input",
				attemptedStatus, rec.Status, MemoryStatusCandidate)
		}
	}
}

func TestMemoryPipelineHappyPathQuarantineValidatePromote(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()

	created, err := ms.WriteCandidate(ctx, MemoryRecord{
		Type: MemoryTypeProcedural, Scope: MemoryScopeProject, Content: "always run tests before merging",
	})
	if err != nil {
		t.Fatalf("WriteCandidate: %v", err)
	}
	if created.Status != MemoryStatusCandidate {
		t.Fatalf("status = %q, want CANDIDATE", created.Status)
	}

	quarantined, err := ms.Quarantine(ctx, created.MemoryID)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if quarantined.Status != MemoryStatusQuarantined {
		t.Fatalf("status = %q, want QUARANTINED", quarantined.Status)
	}

	validated, err := ms.Validate(ctx, created.MemoryID, ValidationDecision{Allowed: true})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if validated.Status != MemoryStatusValidated {
		t.Fatalf("status = %q, want VALIDATED", validated.Status)
	}

	promoted, err := ms.Promote(ctx, created.MemoryID, PromotionDecision{Allowed: true})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if promoted.Status != MemoryStatusActive {
		t.Fatalf("status = %q, want ACTIVE", promoted.Status)
	}

	// Now visible on the governed read path.
	active, err := ms.ListActive(ctx, []string{MemoryScopeProject}, "")
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	found := false
	for _, rec := range active {
		if rec.MemoryID == created.MemoryID {
			found = true
		}
	}
	if !found {
		t.Error("expected the promoted record to be visible via ListActive")
	}
}

func TestMemoryPipelineValidateBlockedWithoutDecision(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()

	created, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: MemoryScopeUser, Content: "x"})
	if err != nil {
		t.Fatalf("WriteCandidate: %v", err)
	}
	if _, err := ms.Quarantine(ctx, created.MemoryID); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}

	_, err = ms.Validate(ctx, created.MemoryID, ValidationDecision{Allowed: false, Reason: "failed safety check"})
	if !errors.Is(err, ErrValidationGateBlocked) {
		t.Fatalf("err = %v, want ErrValidationGateBlocked", err)
	}

	// Confirm the record genuinely did not move.
	rec, err := ms.Get(ctx, created.MemoryID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Status != MemoryStatusQuarantined {
		t.Errorf("status = %q, want QUARANTINED (blocked validation must not change status)", rec.Status)
	}
}

func TestMemoryPipelinePromoteBlockedWithoutDecision(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()

	created, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: MemoryScopeUser, Content: "x"})
	if err != nil {
		t.Fatalf("WriteCandidate: %v", err)
	}
	if _, err := ms.Quarantine(ctx, created.MemoryID); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if _, err := ms.Validate(ctx, created.MemoryID, ValidationDecision{Allowed: true}); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	_, err = ms.Promote(ctx, created.MemoryID, PromotionDecision{Allowed: false, Reason: "no passing eval_required gate"})
	if !errors.Is(err, ErrPromotionGateBlocked) {
		t.Fatalf("err = %v, want ErrPromotionGateBlocked", err)
	}

	rec, err := ms.Get(ctx, created.MemoryID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Status != MemoryStatusValidated {
		t.Errorf("status = %q, want VALIDATED (blocked promotion must not change status)", rec.Status)
	}
}

func TestMemoryPipelineRejectFromEachPreActiveStatus(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()

	advanceTo := func(status string) string {
		created, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: MemoryScopeUser, Content: "x"})
		if err != nil {
			t.Fatalf("WriteCandidate: %v", err)
		}
		switch status {
		case MemoryStatusCandidate:
			return created.MemoryID
		case MemoryStatusQuarantined:
			if _, err := ms.Quarantine(ctx, created.MemoryID); err != nil {
				t.Fatalf("Quarantine: %v", err)
			}
		case MemoryStatusValidated:
			if _, err := ms.Quarantine(ctx, created.MemoryID); err != nil {
				t.Fatalf("Quarantine: %v", err)
			}
			if _, err := ms.Validate(ctx, created.MemoryID, ValidationDecision{Allowed: true}); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		}
		return created.MemoryID
	}

	for _, status := range []string{MemoryStatusCandidate, MemoryStatusQuarantined, MemoryStatusValidated} {
		memoryID := advanceTo(status)
		rejected, err := ms.Reject(ctx, memoryID)
		if err != nil {
			t.Fatalf("Reject from %s: %v", status, err)
		}
		if rejected.Status != MemoryStatusRevoked {
			t.Errorf("from %s: status = %q, want REVOKED", status, rejected.Status)
		}
	}
}

func TestMemoryPipelineInvalidTransitionsAreRejected(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()

	// Cannot skip straight from CANDIDATE to VALIDATED or ACTIVE.
	candidate, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: MemoryScopeUser, Content: "x"})
	if err != nil {
		t.Fatalf("WriteCandidate: %v", err)
	}
	if _, err := ms.Validate(ctx, candidate.MemoryID, ValidationDecision{Allowed: true}); !errors.Is(err, ErrInvalidMemoryTransition) {
		t.Errorf("CANDIDATE -> VALIDATED: err = %v, want ErrInvalidMemoryTransition", err)
	}
	if _, err := ms.Promote(ctx, candidate.MemoryID, PromotionDecision{Allowed: true}); !errors.Is(err, ErrInvalidMemoryTransition) {
		t.Errorf("CANDIDATE -> ACTIVE: err = %v, want ErrInvalidMemoryTransition", err)
	}

	// Cannot move an already-ACTIVE record through this pipeline at all (MEM-005's job).
	if _, err := ms.Quarantine(ctx, candidate.MemoryID); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if _, err := ms.Validate(ctx, candidate.MemoryID, ValidationDecision{Allowed: true}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if _, err := ms.Promote(ctx, candidate.MemoryID, PromotionDecision{Allowed: true}); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if _, err := ms.Reject(ctx, candidate.MemoryID); !errors.Is(err, ErrInvalidMemoryTransition) {
		t.Errorf("ACTIVE -> REVOKED via this pipeline: err = %v, want ErrInvalidMemoryTransition (that's MEM-005)", err)
	}
	if _, err := ms.Quarantine(ctx, candidate.MemoryID); !errors.Is(err, ErrInvalidMemoryTransition) {
		t.Errorf("ACTIVE -> QUARANTINED: err = %v, want ErrInvalidMemoryTransition", err)
	}
}

func TestMemoryPipelineTransitionOnUnknownRecordIsNotFound(t *testing.T) {
	ms := newTestMemoryStore(t)
	if _, err := ms.Quarantine(context.Background(), "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
