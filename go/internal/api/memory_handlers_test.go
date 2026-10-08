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
	srv, _ := newMemoryTestServerWithStore(t)
	return srv
}

func newMemoryTestServerWithStore(t *testing.T) (*httptest.Server, *store.MemoryStore) {
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
	// VRT-AEON-005 T-1: WRAPPED IN auth.Require NOW, and it was not before. These handlers were
	// tested with no authentication middleware at all, which is precisely why a `tenant_id` taken
	// from the request looked acceptable — there was no caller to take one from. A seam tested
	// without the middleware that gives it its identity is a seam tested as if identity were
	// optional.
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)
	return srv, memoryStore
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
	// VRT-AEON-005: authorized like every other helper here. Before this these routes were mounted
	// without auth.Require, so an unauthenticated request was the normal case in these tests.
	resp, err := http.DefaultClient.Do(authorize(req))
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
		"type": "PROCEDURAL", "scope": "user", "content": "run lint before commit",
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

	resp := getAuthed(t, srv.URL+"/memory/active?scope=user")
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
	resp := getAuthed(t, srv.URL+"/memory/00000000-0000-0000-0000-000000000000")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestMemoryHandlersRefuseARequestThatNamesATenant is this test's INVERSION, and the inversion is
// the feature (VRT-AEON-005 T-1).
//
// It used to assert that a GET without a `tenant_id` query value was a 400 — the tenant was
// REQUIRED in the request. That is what made SEC-004's isolation real and the boundary the caller's
// choice: naming the wrong tenant returned 404, and nothing stopped a caller naming any tenant it
// liked. Now the tenant comes from the credential, so a request that omits it is correct and a
// request that names a different one is refused rather than ignored — a client that sends the field
// believes it is choosing.
func TestMemoryHandlersRefuseARequestThatNamesATenant(t *testing.T) {
	srv := newMemoryTestServer(t)
	resp := getAuthed(t, srv.URL+"/memory/00000000-0000-0000-0000-000000000000?tenant_id=someone-else")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403: a request naming a tenant must be told where the tenant comes from", resp.StatusCode)
	}
}

func TestMemoryHandlersListActiveRequiresScope(t *testing.T) {
	srv := newMemoryTestServer(t)
	resp := getAuthed(t, srv.URL+"/memory/active")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestAnotherTenantsMemoryIsIndISTINGUISHABLEFromOneThatDoesNotExist is VRT-AEON-005 T-7, and it
// is the SEC-004 isolation test rewritten around what changed.
//
// The old version created the other tenant's record THROUGH THE API by putting `tenant_id:
// "tenant-A"` in the body, then fetched it with `?tenant_id=tenant-B` and asserted a 404. Every
// line of that was true and the test was weaker than it looked: it proved that naming the wrong
// tenant returns 404, not that a caller cannot name another tenant. The write it used to set up the
// fixture was itself the hole — a caller putting a record into a tenant it does not belong to.
//
// Now the other tenant's record can only be created by going around the API, directly through the
// store, which is the honest shape of the question: the record EXISTS, it belongs to somebody else,
// and every route must behave as though it does not.
func TestAnotherTenantsMemoryIsIndistinguishableFromOneThatDoesNotExist(t *testing.T) {
	srv, memoryStore := newMemoryTestServerWithStore(t)

	// Planted behind the API's back, because the API will no longer let a caller write into another
	// tenant — which is the point.
	foreign, err := memoryStore.WriteCandidate(context.Background(), store.MemoryRecord{
		Type: "SEMANTIC", Scope: "project", TenantID: "another-tenant", Content: "not for this caller",
	})
	if err != nil {
		t.Fatalf("planting the other tenant's record: %v", err)
	}

	// Every route that takes a memory_id. The five after revoke had NO tenant check at all before
	// this change, so a caller could quarantine, promote or repair another tenant's memory knowing
	// only its id — and a memory_id is a uuid, which is not a secret.
	t.Run("GET", func(t *testing.T) {
		resp := getAuthed(t, srv.URL+"/memory/"+foreign.MemoryID)
		defer resp.Body.Close()
		assertLooksMissing(t, resp.StatusCode)
	})
	for _, route := range []string{"/revoke", "/quarantine", "/validate", "/promote", "/reject", "/repair"} {
		t.Run(route, func(t *testing.T) {
			status, body := doJSON(t, http.MethodPost, srv.URL+"/memory/"+foreign.MemoryID+route, map[string]any{"allowed": true})
			assertLooksMissing(t, status)
			if _, leaked := body["content"]; leaked {
				t.Errorf("%s returned the record's content: %v", route, body)
			}
		})
	}

	// NEGATIVE CONTROL: the same routes on the caller's OWN record must work, or the test above
	// passes because nothing works.
	own, err := memoryStore.WriteCandidate(context.Background(), store.MemoryRecord{
		Type: "SEMANTIC", Scope: "project", TenantID: "default", Content: "this caller's own",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := getAuthed(t, srv.URL+"/memory/"+own.MemoryID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the caller's own record returned %d — the test above proves nothing", resp.StatusCode)
	}
}

// assertLooksMissing fixes T-7's exact wording: 404 or an empty list, NEVER 403. A 403 answers a
// question the caller was not entitled to ask — it confirms the record is real.
func assertLooksMissing(t *testing.T, status int) {
	t.Helper()
	if status == http.StatusForbidden {
		t.Errorf("status = 403: a forbidden answer CONFIRMS the record exists. Across a tenant " +
			"boundary the answer has to be indistinguishable from \"no such record\" (T-7)")
		return
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
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
