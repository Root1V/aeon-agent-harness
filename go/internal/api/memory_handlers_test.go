package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aeon-ai/aeon/go/internal/store"
)

// memoryTestDSN mirrors store.testDSN (unexported there): Memory Store HTTP wiring is only
// meaningfully tested against a real Postgres, since the point is proving the handlers correctly
// translate HTTP <-> the real store, not re-testing store-level invariants already covered by
// go/internal/store's own tests.
func memoryTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("AEON_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("AEON_TEST_PG_DSN not set — skipping Postgres integration test (see make test-go-integration)")
	}
	return dsn
}

func newMemoryTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	s, err := store.Connect(ctx, memoryTestDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	memoryStore, err := s.MemoryStore([]byte("test-hmac-key-do-not-use-in-prod"))
	if err != nil {
		t.Fatalf("MemoryStore: %v", err)
	}

	mux := http.NewServeMux()
	(&MemoryHandlers{MemoryStore: memoryStore}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// tamperTestRecordContent mutates a record's content directly via a raw connection, bypassing
// MemoryStore entirely — simulating something other than this store writing to the table (a
// compromised process, a hand-run SQL statement), which is exactly what RepairIfTampered/
// VerifyProvenance must be able to detect (SEC-004).
func tamperTestRecordContent(t *testing.T, memoryID, newContent string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, memoryTestDSN(t))
	if err != nil {
		t.Fatalf("connect for tampering: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `UPDATE memory_records SET content = $1 WHERE memory_id = $2`, newContent, memoryID); err != nil {
		t.Fatalf("tamper: %v", err)
	}
}

func doJSON(t *testing.T, method, url string, body any) (status int, parsed map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, parsed
}

// TestMemoryHandlersWriteCandidateForcesStatusOverHTTP proves write_mode: candidate_only holds
// through the real HTTP surface Reflection (MEM-003) uses, not just at the Go store API.
func TestMemoryHandlersWriteCandidateForcesStatusOverHTTP(t *testing.T) {
	srv := newMemoryTestServer(t)

	status, body := doJSON(t, http.MethodPost, srv.URL+"/memory/candidates", map[string]any{
		"type": "SEMANTIC", "scope": "project", "content": "attempted smuggled write", "status": "ACTIVE",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %v", status, body)
	}
	if body["status"] != "CANDIDATE" {
		t.Errorf("status field = %v, want CANDIDATE regardless of the ACTIVE the request body claimed", body["status"])
	}
}

// TestMemoryHandlersFullPipelineOverHTTP drives quarantine -> validate -> promote entirely over
// real HTTP against a real Postgres, then confirms the record is visible via /memory/active.
func TestMemoryHandlersFullPipelineOverHTTP(t *testing.T) {
	srv := newMemoryTestServer(t)

	status, created := doJSON(t, http.MethodPost, srv.URL+"/memory/candidates", map[string]any{
		"type": "PROCEDURAL", "scope": "user", "tenant_id": "tenant-http-test", "content": "run lint before commit",
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %v", status, created)
	}
	memoryID, _ := created["memory_id"].(string)
	if memoryID == "" {
		t.Fatalf("expected a memory_id in the response, got %v", created)
	}

	status, quarantined := doJSON(t, http.MethodPost, srv.URL+"/memory/"+memoryID+"/quarantine", nil)
	if status != http.StatusOK || quarantined["status"] != "QUARANTINED" {
		t.Fatalf("quarantine: status = %d, body = %v", status, quarantined)
	}

	status, validated := doJSON(t, http.MethodPost, srv.URL+"/memory/"+memoryID+"/validate", map[string]any{"allowed": true})
	if status != http.StatusOK || validated["status"] != "VALIDATED" {
		t.Fatalf("validate: status = %d, body = %v", status, validated)
	}

	status, blocked := doJSON(t, http.MethodPost, srv.URL+"/memory/"+memoryID+"/promote", map[string]any{"allowed": false, "reason": "no eval yet"})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("blocked promote: status = %d, want 422; body = %v", status, blocked)
	}

	status, promoted := doJSON(t, http.MethodPost, srv.URL+"/memory/"+memoryID+"/promote", map[string]any{"allowed": true})
	if status != http.StatusOK || promoted["status"] != "ACTIVE" {
		t.Fatalf("promote: status = %d, body = %v", status, promoted)
	}

	resp, err := http.Get(srv.URL + "/memory/active?scope=user&tenant_id=tenant-http-test")
	if err != nil {
		t.Fatalf("GET /memory/active: %v", err)
	}
	defer resp.Body.Close()
	var active []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&active); err != nil {
		t.Fatalf("decode active list: %v", err)
	}
	found := false
	for _, rec := range active {
		if rec["memory_id"] == memoryID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the promoted record to appear in /memory/active, got %v", active)
	}
}

func TestMemoryHandlersGetUnknownIs404(t *testing.T) {
	srv := newMemoryTestServer(t)
	resp, err := http.Get(srv.URL + "/memory/00000000-0000-0000-0000-000000000000?tenant_id=whatever")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestMemoryHandlersGetRequiresTenantID(t *testing.T) {
	srv := newMemoryTestServer(t)
	resp, err := http.Get(srv.URL + "/memory/00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMemoryHandlersListActiveRequiresScope(t *testing.T) {
	srv := newMemoryTestServer(t)
	resp, err := http.Get(srv.URL + "/memory/active")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestMemoryHandlersGetIsIsolatedByTenant is SEC-004's isolation check on the HTTP surface: a real
// memory_id belonging to tenant A returns 404 — not the record — when fetched with tenant B's
// tenant_id, even though the id itself is perfectly valid.
func TestMemoryHandlersGetIsIsolatedByTenant(t *testing.T) {
	srv := newMemoryTestServer(t)
	status, created := doJSON(t, http.MethodPost, srv.URL+"/memory/candidates", map[string]any{
		"type": "SEMANTIC", "scope": "project", "tenant_id": "tenant-A", "content": "tenant A's secret",
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %v", status, created)
	}
	memoryID := created["memory_id"].(string)

	resp, err := http.Get(srv.URL + "/memory/" + memoryID + "?tenant_id=tenant-B")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-tenant GET: status = %d, want 404 (must not leak that the record exists)", resp.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/memory/" + memoryID + "?tenant_id=tenant-A")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("same-tenant GET: status = %d, want 200", resp2.StatusCode)
	}
}

// TestMemoryHandlersRevokeIsIsolatedByTenant proves the same isolation holds on the write side:
// a caller claiming the wrong tenant cannot revoke (or confirm the existence of) another
// tenant's memory.
func TestMemoryHandlersRevokeIsIsolatedByTenant(t *testing.T) {
	srv := newMemoryTestServer(t)
	status, created := doJSON(t, http.MethodPost, srv.URL+"/memory/candidates", map[string]any{
		"type": "SEMANTIC", "scope": "project", "tenant_id": "tenant-A", "content": "x",
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %v", status, created)
	}
	memoryID := created["memory_id"].(string)
	for _, step := range []string{"/quarantine", "/validate", "/promote"} {
		body := map[string]any{}
		if step != "/quarantine" {
			body["allowed"] = true
		}
		if status, resp := doJSON(t, http.MethodPost, srv.URL+"/memory/"+memoryID+step, body); status != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %v", step, status, resp)
		}
	}

	status, body := doJSON(t, http.MethodPost, srv.URL+"/memory/"+memoryID+"/revoke", map[string]any{"tenant_id": "tenant-B"})
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant revoke: status = %d, want 404; body = %v", status, body)
	}

	status, body = doJSON(t, http.MethodPost, srv.URL+"/memory/"+memoryID+"/revoke", map[string]any{"tenant_id": "tenant-A"})
	if status != http.StatusOK || body["status"] != "REVOKED" {
		t.Fatalf("same-tenant revoke: status = %d, body = %v", status, body)
	}
}

// TestMemoryHandlersRepairDetectsTamperingAndRevokes drives SEC-004's poisoning defense over the
// real HTTP surface: content mutated directly in storage (as if by something other than this
// MemoryStore) is detected by /repair, which revokes the record rather than trust it.
func TestMemoryHandlersRepairDetectsTamperingAndRevokes(t *testing.T) {
	srv := newMemoryTestServer(t)
	status, created := doJSON(t, http.MethodPost, srv.URL+"/memory/candidates", map[string]any{
		"type": "SEMANTIC", "scope": "project", "content": "the real, untampered content",
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %v", status, created)
	}
	memoryID := created["memory_id"].(string)

	status, clean := doJSON(t, http.MethodPost, srv.URL+"/memory/"+memoryID+"/repair", nil)
	if status != http.StatusOK || clean["tampered"] != false {
		t.Fatalf("repair on untampered record: status = %d, body = %v, want tampered=false", status, clean)
	}

	// Simulate tampering: mutate content directly, the way a compromised process with raw DB
	// access (not this MemoryStore) would — hash/provenance_hmac are now stale.
	tamperTestRecordContent(t, memoryID, "attacker-controlled content")

	status, repaired := doJSON(t, http.MethodPost, srv.URL+"/memory/"+memoryID+"/repair", nil)
	if status != http.StatusOK || repaired["tampered"] != true {
		t.Fatalf("repair on tampered record: status = %d, body = %v, want tampered=true", status, repaired)
	}
	memory, ok := repaired["memory"].(map[string]any)
	if !ok || memory["status"] != "REVOKED" {
		t.Errorf("expected the tampered record to come back REVOKED, got %v", repaired["memory"])
	}
}
