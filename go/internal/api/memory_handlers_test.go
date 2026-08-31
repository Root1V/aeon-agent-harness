package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

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
	resp, err := http.Get(srv.URL + "/memory/00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
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
