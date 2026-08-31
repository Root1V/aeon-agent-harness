package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func newTestMemoryStore(t *testing.T) *MemoryStore {
	t.Helper()
	s := newTestStore(t)
	ms, err := s.MemoryStore([]byte("test-hmac-key-do-not-use-in-prod"))
	if err != nil {
		t.Fatalf("MemoryStore: %v", err)
	}
	return ms
}

func memoryRecordSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// this file: go/internal/store/memory_store_test.go -> repo root is three levels up.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	schemaPath := filepath.Join(repoRoot, "proto", "schemas", "memory_record.schema.json")

	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("reading memory_record.schema.json: %v", err)
	}
	var meta struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("reading $id: %v", err)
	}

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decoding memory_record.schema.json: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(meta.ID, doc); err != nil {
		t.Fatalf("registering memory_record.schema.json: %v", err)
	}
	schema, err := compiler.Compile(meta.ID)
	if err != nil {
		t.Fatalf("compiling memory_record.schema.json: %v", err)
	}
	return schema
}

// validateAgainstMemoryRecordSchema checks rec against memory_record.schema.json. The schema
// (additionalProperties: false) only knows the wire contract's fields, not created_at/updated_at
// (registry-only bookkeeping the schema never declared), so those two are stripped before
// validating — the same shape MEM-002's promotion API would actually hand to a caller.
func validateAgainstMemoryRecordSchema(t *testing.T, rec *MemoryRecord) error {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	delete(asMap, "created_at")
	delete(asMap, "updated_at")
	raw, err = json.Marshal(asMap)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}

	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decoding record for validation: %v", err)
	}
	return memoryRecordSchema(t).Validate(instance)
}

// TestMemoryRecordSchemaValid is MEM-001's acceptance test: a record written through the real
// MemoryStore against a real Postgres — with its hash and provenance_hmac computed by the store,
// not supplied by the caller — round-trips as a document that is genuinely valid against
// proto/schemas/memory_record.schema.json, the same contract MEM-002..MEM-005 and SEC-004 build on.
func TestMemoryRecordSchemaValid(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()

	created, err := ms.Create(ctx, MemoryRecord{
		Type:         MemoryTypeSemantic,
		Scope:        MemoryScopeProject,
		TenantID:     "tenant-a",
		Content:      "The user prefers concise commit messages.",
		SourceRunIDs: []string{uuid.NewString()},
		EvidenceRefs: []string{uuid.NewString()},
		TrustLevel:   TrustLevelCandidate,
		Confidence:   0.8,
		UtilityScore: 0,
		Status:       MemoryStatusCandidate,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := validateAgainstMemoryRecordSchema(t, created); err != nil {
		t.Fatalf("created record is not schema-valid: %v", err)
	}

	if created.Hash == "" {
		t.Error("expected a non-empty content hash")
	}
	if created.ProvenanceHMAC == "" {
		t.Error("expected a non-empty provenance_hmac")
	}
	if created.Version != 1 {
		t.Errorf("version = %d, want 1 for a freshly created record", created.Version)
	}
	if _, err := uuid.Parse(created.MemoryID); err != nil {
		t.Errorf("memory_id = %q is not a well-formed uuid: %v", created.MemoryID, err)
	}
}

// TestMemoryStoreCreateOverwritesCallerSuppliedHash proves hash/provenance_hmac are always
// computed by the store — a caller cannot forge them to make tampered content look genuine.
func TestMemoryStoreCreateOverwritesCallerSuppliedHash(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()

	created, err := ms.Create(ctx, MemoryRecord{
		Type:           MemoryTypeEpisodic,
		Scope:          MemoryScopeUser,
		Content:        "real content",
		Status:         MemoryStatusCandidate,
		Hash:           "forged-hash",
		ProvenanceHMAC: "forged-hmac",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Hash == "forged-hash" || created.ProvenanceHMAC == "forged-hmac" {
		t.Fatalf("caller-supplied hash/provenance_hmac must be overwritten by the store, got hash=%q provenance_hmac=%q",
			created.Hash, created.ProvenanceHMAC)
	}
	if !ms.VerifyProvenance(created) {
		t.Error("a record straight from Create must verify against its own store")
	}
}

// TestMemoryStoreCreateRejectsDirectActiveWrite proves the invariant memory_record.schema.json
// documents in its own description: a caller can never write a record directly into ACTIVE (or
// any post-candidate status) — only MEM-002's pipeline can move it there.
func TestMemoryStoreCreateRejectsDirectActiveWrite(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()

	for _, status := range []string{MemoryStatusValidated, MemoryStatusActive, MemoryStatusSuperseded, MemoryStatusRevoked} {
		_, err := ms.Create(ctx, MemoryRecord{
			Type:    MemoryTypeSemantic,
			Scope:   MemoryScopeSession,
			Content: "should not be writable at status " + status,
			Status:  status,
		})
		if !errors.Is(err, ErrDirectActiveWriteRejected) {
			t.Errorf("status %q: err = %v, want ErrDirectActiveWriteRejected", status, err)
		}
	}
}

func TestMemoryStoreCreateRejectsInvalidEnums(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()

	cases := []MemoryRecord{
		{Type: "NOT_A_TYPE", Scope: MemoryScopeSession, Content: "x", Status: MemoryStatusCandidate},
		{Type: MemoryTypeSemantic, Scope: "not-a-scope", Content: "x", Status: MemoryStatusCandidate},
		{Type: MemoryTypeSemantic, Scope: MemoryScopeSession, Content: "", Status: MemoryStatusCandidate},
		{Type: MemoryTypeSemantic, Scope: MemoryScopeSession, Content: "x", Status: MemoryStatusCandidate, Confidence: 1.5},
	}
	for i, rec := range cases {
		if _, err := ms.Create(ctx, rec); !errors.Is(err, ErrInvalidMemoryRecord) {
			t.Errorf("case %d: err = %v, want ErrInvalidMemoryRecord", i, err)
		}
	}
}

// TestMemoryStoreListActiveRespectsScopeTenantAndTTL proves the governed read path: only ACTIVE,
// non-expired records in a requested scope+tenant come back, even though CANDIDATE, wrong-tenant,
// wrong-scope, and expired-ACTIVE rows all exist in the same table.
func TestMemoryStoreListActiveRespectsScopeTenantAndTTL(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Reach into the pool directly to insert rows Create() would never allow (ACTIVE, expired
	// ACTIVE) — ListActive's filtering is what this test verifies, not how a row gets to ACTIVE.
	insertActive := func(id, scope, tenantID string, expiresAt *time.Time) {
		_, err := s.pool.Exec(ctx,
			`INSERT INTO memory_records
				(memory_id, type, scope, tenant_id, content, source_run_ids, evidence_refs,
				 status, expires_at, version, hash, provenance_hmac)
			 VALUES ($1, 'SEMANTIC', $2, $3, 'content', '[]', '[]', 'ACTIVE', $4, 1, 'h', 'p')`,
			id, scope, tenantID, expiresAt,
		)
		if err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}

	tenantA := "tenant-" + uuid.NewString()
	wantVisible := uuid.NewString()
	wrongTenant := uuid.NewString()
	wrongScope := uuid.NewString()
	expired := uuid.NewString()
	past := time.Now().Add(-time.Hour)

	insertActive(wantVisible, MemoryScopeProject, tenantA, nil)
	insertActive(wrongTenant, MemoryScopeProject, "tenant-other", nil)
	insertActive(wrongScope, MemoryScopeOrg, tenantA, nil)
	insertActive(expired, MemoryScopeProject, tenantA, &past)

	ms, err := s.MemoryStore([]byte("test-key"))
	if err != nil {
		t.Fatalf("MemoryStore: %v", err)
	}
	_, err = ms.Create(ctx, MemoryRecord{
		Type: MemoryTypeSemantic, Scope: MemoryScopeProject, TenantID: tenantA,
		Content: "still a candidate", Status: MemoryStatusCandidate,
	})
	if err != nil {
		t.Fatalf("seed candidate: %v", err)
	}

	got, err := ms.ListActive(ctx, []string{MemoryScopeProject, MemoryScopeUser}, tenantA)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}

	ids := make(map[string]bool, len(got))
	for _, rec := range got {
		ids[rec.MemoryID] = true
	}
	if !ids[wantVisible] {
		t.Errorf("expected the matching ACTIVE record %s to be visible", wantVisible)
	}
	for _, excluded := range []string{wrongTenant, wrongScope, expired} {
		if ids[excluded] {
			t.Errorf("record %s should have been excluded (wrong tenant/scope/expired), but was returned", excluded)
		}
	}
	if len(got) != 1 {
		t.Errorf("len(got) = %d, want exactly 1 (only wantVisible)", len(got))
	}
}

func TestMemoryStoreVerifyProvenanceDetectsTampering(t *testing.T) {
	ms := newTestMemoryStore(t)
	ctx := context.Background()

	created, err := ms.Create(ctx, MemoryRecord{
		Type: MemoryTypeConstraint, Scope: MemoryScopeSession, Content: "never delete production data",
		Status: MemoryStatusQuarantined,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !ms.VerifyProvenance(created) {
		t.Fatal("a freshly created record must verify")
	}

	tampered := *created
	tampered.Content = "it's fine to delete production data"
	if ms.VerifyProvenance(&tampered) {
		t.Error("VerifyProvenance must detect content tampered with outside the store")
	}

	tamperedHMAC := *created
	tamperedHMAC.ProvenanceHMAC = "not-the-real-hmac"
	if ms.VerifyProvenance(&tamperedHMAC) {
		t.Error("VerifyProvenance must detect a forged provenance_hmac")
	}
}

func TestMemoryStoreGetNotFound(t *testing.T) {
	ms := newTestMemoryStore(t)
	_, err := ms.Get(context.Background(), uuid.NewString())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestNewMemoryStoreRejectsEmptyKey(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.MemoryStore(nil); err == nil {
		t.Fatal("expected an error constructing a MemoryStore with an empty HMAC key")
	}
}
