package store

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// promoteToActive drives a fresh candidate all the way to ACTIVE via the real MEM-002 pipeline,
// returning its memory_id — the fixture every MEM-005 test starts from, since Prune/Supersede/
// Revoke/RecordUsage all only ever operate on an ACTIVE record.
func promoteToActive(t *testing.T, ms *MemoryStore, ctx context.Context, scope string) string {
	t.Helper()
	created, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: scope, Content: "x"})
	if err != nil {
		t.Fatalf("WriteCandidate: %v", err)
	}
	if _, err := ms.Quarantine(ctx, created.MemoryID); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if _, err := ms.Validate(ctx, created.MemoryID, ValidationDecision{Allowed: true}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if _, err := ms.Promote(ctx, created.MemoryID, PromotionDecision{Allowed: true}); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	return created.MemoryID
}

func TestDecayedUtilityHalvesAfterOneHalfLifeAndUnchangedBeforeItStarts(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	got := DecayedUtility(1.0, since, since.Add(10*24*time.Hour), 10)
	if math.Abs(got-0.5) > 1e-9 {
		t.Errorf("after exactly one half-life: got %v, want 0.5", got)
	}

	if got := DecayedUtility(1.0, since, since, 10); got != 1.0 {
		t.Errorf("no time elapsed: got %v, want 1.0 unchanged", got)
	}
	if got := DecayedUtility(1.0, since, since.Add(-24*time.Hour), 10); got != 1.0 {
		t.Errorf("asOf before since: got %v, want 1.0 unchanged", got)
	}
	if got := DecayedUtility(1.0, since, since.Add(10*24*time.Hour), 0); got != 1.0 {
		t.Errorf("non-positive half-life: got %v, want 1.0 unchanged (no decay model)", got)
	}
}

// TestMemoryDecayPrunesStale is MEM-005's acceptance test: a real ACTIVE record whose usage is
// backdated far enough that its utility_score has decayed below threshold gets pruned (REVOKED)
// by a real Prune call against real Postgres, and disappears from the governed ListActive view.
func TestMemoryDecayPrunesStale(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()
	tenantID := "tenant-decay-" + time.Now().Format("150405.000000000")

	memoryID := promoteToActive(t, ms, ctx, MemoryScopeProject)
	if _, err := ms.pool.Exec(ctx, `UPDATE memory_records SET tenant_id = $1 WHERE memory_id = $2`, tenantID, memoryID); err != nil {
		t.Fatalf("seed tenant_id: %v", err)
	}
	if _, err := ms.RecordUsage(ctx, memoryID, true); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// Backdate last_used_at 30 days into the past: with a 10-day half-life, decayed utility is
	// 1.0 * 0.5^3 = 0.125 — well below the 0.4 threshold used below.
	backdated := time.Now().Add(-30 * 24 * time.Hour)
	if _, err := ms.pool.Exec(ctx, `UPDATE memory_records SET last_used_at = $1 WHERE memory_id = $2`, backdated, memoryID); err != nil {
		t.Fatalf("backdate last_used_at: %v", err)
	}

	pruned, err := ms.Prune(ctx, []string{MemoryScopeProject}, tenantID, 10, 0.4, time.Now())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != memoryID {
		t.Fatalf("pruned = %v, want [%s]", pruned, memoryID)
	}

	rec, err := ms.Get(ctx, memoryID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Status != MemoryStatusRevoked {
		t.Errorf("status = %q, want REVOKED after pruning", rec.Status)
	}

	active, err := ms.ListActive(ctx, []string{MemoryScopeProject}, tenantID)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	for _, r := range active {
		if r.MemoryID == memoryID {
			t.Error("pruned record must no longer appear in ListActive")
		}
	}
}

func TestMemoryPruneKeepsFreshOrHighUtility(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()
	tenantID := "tenant-fresh-" + time.Now().Format("150405.000000000")

	memoryID := promoteToActive(t, ms, ctx, MemoryScopeUser)
	if _, err := ms.pool.Exec(ctx, `UPDATE memory_records SET tenant_id = $1 WHERE memory_id = $2`, tenantID, memoryID); err != nil {
		t.Fatalf("seed tenant_id: %v", err)
	}
	if _, err := ms.RecordUsage(ctx, memoryID, true); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	// last_used_at is "now" (never backdated) — with any reasonable half-life, decay since a
	// moment ago is negligible, so this must survive even a demanding threshold.
	pruned, err := ms.Prune(ctx, []string{MemoryScopeUser}, tenantID, 10, 0.9, time.Now())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(pruned) != 0 {
		t.Fatalf("pruned = %v, want none — this record was just used", pruned)
	}

	rec, err := ms.Get(ctx, memoryID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Status != MemoryStatusActive {
		t.Errorf("status = %q, want still ACTIVE", rec.Status)
	}
}

func TestMemoryRecordUsageAdjustsUtilityScoreAndFloorsAtZero(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()
	memoryID := promoteToActive(t, ms, ctx, MemoryScopeSession)

	rec, err := ms.RecordUsage(ctx, memoryID, true)
	if err != nil {
		t.Fatalf("RecordUsage(success): %v", err)
	}
	if rec.UtilityScore != usageSuccessDelta {
		t.Fatalf("utility_score after one success = %v, want %v", rec.UtilityScore, usageSuccessDelta)
	}
	if rec.LastUsedAt == nil {
		t.Fatal("expected last_used_at to be set after RecordUsage")
	}

	rec, err = ms.RecordUsage(ctx, memoryID, false)
	if err != nil {
		t.Fatalf("RecordUsage(failure): %v", err)
	}
	if want := usageSuccessDelta + usageFailureDelta; rec.UtilityScore != want {
		t.Fatalf("utility_score after success+failure = %v, want %v", rec.UtilityScore, want)
	}

	// Drive it well below zero with repeated failures — must floor at 0, never go negative
	// (mirrors the schema's own utility_score minimum:0).
	for i := 0; i < 10; i++ {
		rec, err = ms.RecordUsage(ctx, memoryID, false)
		if err != nil {
			t.Fatalf("RecordUsage(failure) #%d: %v", i, err)
		}
	}
	if rec.UtilityScore != 0 {
		t.Errorf("utility_score = %v, want floored at 0", rec.UtilityScore)
	}
}

func TestMemoryRecordUsageRejectsNonActiveRecord(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()
	created, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: MemoryScopeSession, Content: "x"})
	if err != nil {
		t.Fatalf("WriteCandidate: %v", err)
	}
	if _, err := ms.RecordUsage(ctx, created.MemoryID, true); !errors.Is(err, ErrInvalidMemoryTransition) {
		t.Fatalf("err = %v, want ErrInvalidMemoryTransition (record is still CANDIDATE)", err)
	}
}

func TestMemorySupersedeMovesActiveToSupersededAndLinksNewID(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()

	oldID := promoteToActive(t, ms, ctx, MemoryScopeProject)
	newCandidate, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: MemoryScopeProject, Content: "an updated version of the same fact"})
	if err != nil {
		t.Fatalf("WriteCandidate: %v", err)
	}

	superseded, err := ms.Supersede(ctx, oldID, newCandidate.MemoryID)
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if superseded.Status != MemoryStatusSuperseded {
		t.Errorf("status = %q, want SUPERSEDED", superseded.Status)
	}
	if superseded.SupersededBy != newCandidate.MemoryID {
		t.Errorf("superseded_by = %q, want %q", superseded.SupersededBy, newCandidate.MemoryID)
	}
}

func TestMemorySupersedeRejectsNonActiveSource(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()
	created, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: MemoryScopeSession, Content: "x"})
	if err != nil {
		t.Fatalf("WriteCandidate: %v", err)
	}
	if _, err := ms.Supersede(ctx, created.MemoryID, "irrelevant"); !errors.Is(err, ErrInvalidMemoryTransition) {
		t.Fatalf("err = %v, want ErrInvalidMemoryTransition (record is still CANDIDATE, not ACTIVE)", err)
	}
}

func TestMemoryRevokeMovesActiveToRevoked(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()
	memoryID := promoteToActive(t, ms, ctx, MemoryScopeOrg)

	revoked, err := ms.Revoke(ctx, memoryID)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked.Status != MemoryStatusRevoked {
		t.Errorf("status = %q, want REVOKED", revoked.Status)
	}
}

func TestMemoryRevokeRejectsNonActiveSource(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()
	created, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: MemoryScopeSession, Content: "x"})
	if err != nil {
		t.Fatalf("WriteCandidate: %v", err)
	}
	// MEM-002's Reject (not MEM-005's Revoke) is the correct way to invalidate a pre-ACTIVE
	// candidate — Revoke is scoped to already-ACTIVE memories only.
	if _, err := ms.Revoke(ctx, created.MemoryID); !errors.Is(err, ErrInvalidMemoryTransition) {
		t.Fatalf("err = %v, want ErrInvalidMemoryTransition", err)
	}
}
