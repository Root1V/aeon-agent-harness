package store

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
)

// memoryPoisoningCase mirrors one line of evals/datasets/memory_poisoning.jsonl.
type memoryPoisoningCase struct {
	ID              string `json:"id"`
	Attack          string `json:"attack"`
	ExpectedDefense string `json:"expected_defense"`
}

func loadMemoryPoisoningDataset(t *testing.T) []memoryPoisoningCase {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// this file: go/internal/store/memory_poisoning_test.go -> repo root is three levels up.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	datasetPath := filepath.Join(repoRoot, "evals", "datasets", "memory_poisoning.jsonl")

	f, err := os.Open(datasetPath)
	if err != nil {
		t.Fatalf("opening %s: %v", datasetPath, err)
	}
	defer f.Close()

	var cases []memoryPoisoningCase
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var c memoryPoisoningCase
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("parsing dataset line %q: %v", line, err)
		}
		cases = append(cases, c)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading dataset: %v", err)
	}
	return cases
}

// memoryPoisoningDefenses maps every case id evals/datasets/memory_poisoning.jsonl declares to the
// real subtest that proves the defense holds. Deliberately a hard requirement, not a lookup that
// silently skips a missing entry: a dataset case with no implementation here must fail the test,
// not report an inflated pass (the roadmap's own "no silent caps" rule, applied to a security
// suite where silent gaps are the worst place to have one).
var memoryPoisoningDefenses = map[string]func(t *testing.T, ms *MemoryStore, ctx context.Context){
	"direct_active_write":                 testDirectActiveWriteIsForced,
	"tamper_content_bypass_hmac":          testTamperContentBypassHMAC,
	"cross_tenant_read_by_id":             testCrossTenantReadByID,
	"cross_tenant_list_active":            testCrossTenantListActive,
	"cross_tenant_revoke":                 testCrossTenantRevoke,
	"revocation_removes_from_active_view": testRevocationRemovesFromActiveView,
}

// TestMemoryPoisoningSuite is SEC-004's acceptance suite: every attack scenario named in
// evals/datasets/memory_poisoning.jsonl (the same config-as-code EvalSuite `aeon eval list` shows
// as memory_poisoning) is exercised for real against a real Postgres-backed MemoryStore.
func TestMemoryPoisoningSuite(t *testing.T) {
	cases := loadMemoryPoisoningDataset(t)
	if len(cases) == 0 {
		t.Fatal("memory_poisoning.jsonl produced zero cases — dataset is empty or unreadable")
	}

	for _, c := range cases {
		defense, ok := memoryPoisoningDefenses[c.ID]
		if !ok {
			t.Fatalf("dataset case %q (%s) has no implemented defense test — see memoryPoisoningDefenses", c.ID, c.Attack)
		}
		t.Run(c.ID, func(t *testing.T) {
			ms := newTestMemoryStore(t)
			defense(t, ms, context.Background())
		})
	}
}

func testDirectActiveWriteIsForced(t *testing.T, ms *MemoryStore, ctx context.Context) {
	for _, attempted := range []string{MemoryStatusValidated, MemoryStatusActive, MemoryStatusSuperseded, MemoryStatusRevoked} {
		rec, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: MemoryScopeSession, Content: "x", Status: attempted})
		if err != nil {
			t.Fatalf("WriteCandidate (attempted %q): %v", attempted, err)
		}
		if rec.Status != MemoryStatusCandidate {
			t.Errorf("attempted %q: got status %q, want CANDIDATE", attempted, rec.Status)
		}
	}
}

func testTamperContentBypassHMAC(t *testing.T, ms *MemoryStore, ctx context.Context) {
	created, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: MemoryScopeSession, Content: "the real content"})
	if err != nil {
		t.Fatalf("WriteCandidate: %v", err)
	}

	tampered, _, err := ms.RepairIfTampered(ctx, created.MemoryID)
	if err != nil {
		t.Fatalf("RepairIfTampered (untampered): %v", err)
	}
	if tampered {
		t.Fatal("an untampered, freshly created record must not be reported as tampered")
	}

	if _, err := ms.pool.Exec(ctx, `UPDATE memory_records SET content = $1 WHERE memory_id = $2`, "attacker-controlled content", created.MemoryID); err != nil {
		t.Fatalf("simulate tampering: %v", err)
	}

	tampered, repaired, err := ms.RepairIfTampered(ctx, created.MemoryID)
	if err != nil {
		t.Fatalf("RepairIfTampered (tampered): %v", err)
	}
	if !tampered {
		t.Fatal("tampering must be detected")
	}
	if repaired.Status != MemoryStatusRevoked {
		t.Errorf("status after repair = %q, want REVOKED", repaired.Status)
	}
}

func testCrossTenantReadByID(t *testing.T, ms *MemoryStore, ctx context.Context) {
	created, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: MemoryScopeProject, TenantID: "tenant-A", Content: "tenant A's secret"})
	if err != nil {
		t.Fatalf("WriteCandidate: %v", err)
	}
	// The store's own Get is memory_id-keyed (used internally by the pipeline, which has no
	// per-request tenant context) — isolation for an external caller is enforced at the HTTP
	// boundary (go/internal/api/memory_handlers.go's getMemory), tested there
	// (TestMemoryHandlersGetIsIsolatedByTenant). Here we confirm the record really does carry the
	// tenant_id that boundary check depends on.
	got, err := ms.Get(ctx, created.MemoryID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TenantID != "tenant-A" {
		t.Fatalf("tenant_id = %q, want tenant-A", got.TenantID)
	}
}

func testCrossTenantListActive(t *testing.T, ms *MemoryStore, ctx context.Context) {
	memoryID := promoteToActive(t, ms, ctx, MemoryScopeProject)
	if _, err := ms.pool.Exec(ctx, `UPDATE memory_records SET tenant_id = 'tenant-A' WHERE memory_id = $1`, memoryID); err != nil {
		t.Fatalf("seed tenant_id: %v", err)
	}

	leaked, err := ms.ListActive(ctx, []string{MemoryScopeProject}, "tenant-B")
	if err != nil {
		t.Fatalf("ListActive as tenant-B: %v", err)
	}
	for _, rec := range leaked {
		if rec.MemoryID == memoryID {
			t.Fatal("tenant-A's memory must never appear in tenant-B's ListActive result")
		}
	}

	visible, err := ms.ListActive(ctx, []string{MemoryScopeProject}, "tenant-A")
	if err != nil {
		t.Fatalf("ListActive as tenant-A: %v", err)
	}
	found := false
	for _, rec := range visible {
		if rec.MemoryID == memoryID {
			found = true
		}
	}
	if !found {
		t.Fatal("tenant-A must still see its own memory")
	}
}

func testCrossTenantRevoke(t *testing.T, ms *MemoryStore, ctx context.Context) {
	// The store's Revoke itself is memory_id-keyed, same reasoning as testCrossTenantReadByID;
	// the tenant check for an external caller lives at the HTTP boundary, covered by
	// TestMemoryHandlersRevokeIsIsolatedByTenant. Here we confirm Revoke only ever succeeds from
	// ACTIVE, so a caller can't sidestep the boundary check by hitting some other state.
	created, err := ms.WriteCandidate(ctx, MemoryRecord{Type: MemoryTypeSemantic, Scope: MemoryScopeSession, Content: "x"})
	if err != nil {
		t.Fatalf("WriteCandidate: %v", err)
	}
	if _, err := ms.Revoke(ctx, created.MemoryID); err == nil {
		t.Fatal("Revoke must refuse a non-ACTIVE record regardless of tenant")
	}
}

func testRevocationRemovesFromActiveView(t *testing.T, ms *MemoryStore, ctx context.Context) {
	tenantID := "tenant-" + uuid.NewString()
	memoryID := promoteToActive(t, ms, ctx, MemoryScopeUser)
	if _, err := ms.pool.Exec(ctx, `UPDATE memory_records SET tenant_id = $1 WHERE memory_id = $2`, tenantID, memoryID); err != nil {
		t.Fatalf("seed tenant_id: %v", err)
	}

	if _, err := ms.Revoke(ctx, memoryID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	active, err := ms.ListActive(ctx, []string{MemoryScopeUser}, tenantID)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	for _, rec := range active {
		if rec.MemoryID == memoryID {
			t.Fatal("a revoked memory must not appear in ListActive")
		}
	}
}
