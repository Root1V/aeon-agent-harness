package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Memory types (MEM-001), mirroring proto/schemas/memory_record.schema.json's type enum.
const (
	MemoryTypeEpisodic   = "EPISODIC"
	MemoryTypeSemantic   = "SEMANTIC"
	MemoryTypeProcedural = "PROCEDURAL"
	MemoryTypeConstraint = "CONSTRAINT"
)

// Memory scopes, mirroring the schema's scope enum. AgentManifest.spec.memoryPolicy.readScopes
// (see the spec's example manifest) names a subset of these to read from.
const (
	MemoryScopeSession = "session"
	MemoryScopeUser    = "user"
	MemoryScopeProject = "project"
	MemoryScopeTenant  = "tenant"
	MemoryScopeOrg     = "org"
)

// Memory statuses, mirroring the schema's status enum. Only MemoryStatusCandidate and
// MemoryStatusQuarantined are reachable via Create — everything after that is the
// quarantine->validate->promote/reject pipeline (MEM-002), which is the only path that may ever
// move a record to MemoryStatusActive.
const (
	MemoryStatusCandidate   = "CANDIDATE"
	MemoryStatusQuarantined = "QUARANTINED"
	MemoryStatusValidated   = "VALIDATED"
	MemoryStatusActive      = "ACTIVE"
	MemoryStatusSuperseded  = "SUPERSEDED"
	MemoryStatusRevoked     = "REVOKED"
)

// Trust levels, mirroring the schema's trust_level enum.
const (
	TrustLevelUntrusted = "UNTRUSTED"
	TrustLevelCandidate = "CANDIDATE"
	TrustLevelValidated = "VALIDATED"
	TrustLevelTrusted   = "TRUSTED"
)

// ErrInvalidMemoryRecord is returned when a record fails MEM-001's structural validation
// (missing required field, or a value outside its schema enum).
var ErrInvalidMemoryRecord = errors.New("store: invalid memory record")

// ErrDirectActiveWriteRejected is returned when Create is asked to write a status other than
// CANDIDATE or QUARANTINED. This is the invariant memory_record.schema.json documents in its own
// description: "direct writes to ACTIVE status are rejected by the API by construction." Reaching
// VALIDATED/ACTIVE/SUPERSEDED/REVOKED requires the candidate pipeline (MEM-002).
var ErrDirectActiveWriteRejected = errors.New("store: memory records may only be created as CANDIDATE or QUARANTINED")

// MemoryRecord is a governed unit of long-term memory (MEM-001), mirroring
// proto/schemas/memory_record.schema.json field-for-field.
type MemoryRecord struct {
	MemoryID       string     `json:"memory_id"`
	Type           string     `json:"type"`
	Scope          string     `json:"scope"`
	TenantID       string     `json:"tenant_id"`
	Content        string     `json:"content"`
	SourceRunIDs   []string   `json:"source_run_ids"`
	EvidenceRefs   []string   `json:"evidence_refs"`
	TrustLevel     string     `json:"trust_level,omitempty"`
	Confidence     float64    `json:"confidence"`
	UtilityScore   float64    `json:"utility_score"`
	Status         string     `json:"status"`
	ExpiresAt      *time.Time `json:"expires_at"`
	Version        int        `json:"version"`
	Hash           string     `json:"hash"`
	ProvenanceHMAC string     `json:"provenance_hmac"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// MemoryStore is the Postgres-backed store for MemoryRecords. hash and provenance_hmac are
// always computed here, from hmacKey — never trusted from a caller — so a record's integrity can
// later be verified independently of whoever wrote it (VerifyProvenance; the fuller poisoning
// defense is SEC-004).
type MemoryStore struct {
	pool    *pgxpool.Pool
	hmacKey []byte
}

// NewMemoryStore builds a MemoryStore. hmacKey must be non-empty: provenance is meaningless
// without a real key, so an empty key is a construction error, not a silently-weakened runtime.
func NewMemoryStore(pool *pgxpool.Pool, hmacKey []byte) (*MemoryStore, error) {
	if len(hmacKey) == 0 {
		return nil, fmt.Errorf("store: memory store requires a non-empty HMAC key (see AEON_MEMORY_HMAC_KEY)")
	}
	return &MemoryStore{pool: pool, hmacKey: hmacKey}, nil
}

// MemoryStore returns a MemoryStore handle sharing this Store's pool.
func (s *Store) MemoryStore(hmacKey []byte) (*MemoryStore, error) {
	return NewMemoryStore(s.pool, hmacKey)
}

// Create validates rec, computes its hash and provenance_hmac (overwriting whatever the caller
// supplied), and persists it. memory_id and version are assigned here if the caller left them
// zero-valued.
func (s *MemoryStore) Create(ctx context.Context, rec MemoryRecord) (*MemoryRecord, error) {
	if rec.MemoryID == "" {
		rec.MemoryID = uuid.NewString()
	}
	if rec.Version == 0 {
		rec.Version = 1
	}
	if rec.SourceRunIDs == nil {
		rec.SourceRunIDs = []string{}
	}
	if rec.EvidenceRefs == nil {
		rec.EvidenceRefs = []string{}
	}
	if err := validateMemoryRecord(rec); err != nil {
		return nil, err
	}
	if rec.Status != MemoryStatusCandidate && rec.Status != MemoryStatusQuarantined {
		return nil, fmt.Errorf("%w: got %q", ErrDirectActiveWriteRejected, rec.Status)
	}

	rec.Hash = contentHash(rec.Content)
	rec.ProvenanceHMAC = s.provenanceHMAC(rec.MemoryID, rec.Hash, rec.SourceRunIDs, rec.EvidenceRefs)

	sourceRunIDsJSON, err := json.Marshal(rec.SourceRunIDs)
	if err != nil {
		return nil, fmt.Errorf("store: marshal source_run_ids: %w", err)
	}
	evidenceRefsJSON, err := json.Marshal(rec.EvidenceRefs)
	if err != nil {
		return nil, fmt.Errorf("store: marshal evidence_refs: %w", err)
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO memory_records
			(memory_id, type, scope, tenant_id, content, source_run_ids, evidence_refs,
			 trust_level, confidence, utility_score, status, expires_at, version, hash, provenance_hmac)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		rec.MemoryID, rec.Type, rec.Scope, rec.TenantID, rec.Content, sourceRunIDsJSON, evidenceRefsJSON,
		nullableString(rec.TrustLevel), rec.Confidence, rec.UtilityScore, rec.Status, rec.ExpiresAt, rec.Version,
		rec.Hash, rec.ProvenanceHMAC,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: memory %s", ErrAlreadyExists, rec.MemoryID)
		}
		return nil, fmt.Errorf("store: create memory record: %w", err)
	}
	return s.Get(ctx, rec.MemoryID)
}

// Get fetches a single memory record by id.
func (s *MemoryStore) Get(ctx context.Context, memoryID string) (*MemoryRecord, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT memory_id, type, scope, tenant_id, content, source_run_ids, evidence_refs,
		        trust_level, confidence, utility_score, status, expires_at, version, hash,
		        provenance_hmac, created_at, updated_at
		 FROM memory_records WHERE memory_id = $1`,
		memoryID,
	)
	return scanMemoryRow(row)
}

// ListActive returns non-expired ACTIVE memory records visible to any of scopes, for tenantID —
// the governed read path a running agent uses (AgentManifest.spec.memoryPolicy.readScopes in the
// spec's example manifest). Anything CANDIDATE/QUARANTINED/VALIDATED/SUPERSEDED/REVOKED, or past
// its expires_at, is invisible here even though it still exists in the table.
func (s *MemoryStore) ListActive(ctx context.Context, scopes []string, tenantID string) ([]*MemoryRecord, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT memory_id, type, scope, tenant_id, content, source_run_ids, evidence_refs,
		        trust_level, confidence, utility_score, status, expires_at, version, hash,
		        provenance_hmac, created_at, updated_at
		 FROM memory_records
		 WHERE status = $1 AND tenant_id = $2 AND scope = ANY($3) AND (expires_at IS NULL OR expires_at > now())
		 ORDER BY created_at DESC`,
		MemoryStatusActive, tenantID, scopes,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list active memory records: %w", err)
	}
	defer rows.Close()

	var out []*MemoryRecord
	for rows.Next() {
		rec, err := scanMemoryRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// VerifyProvenance recomputes rec's hash and provenance_hmac from its own content/refs and this
// store's key, and reports whether they still match what's on the record. A mismatch means the
// content or a provenance field was altered by something other than this MemoryStore — the
// signal a poisoning defense (SEC-004) would act on.
func (s *MemoryStore) VerifyProvenance(rec *MemoryRecord) bool {
	if rec.Hash != contentHash(rec.Content) {
		return false
	}
	return rec.ProvenanceHMAC == s.provenanceHMAC(rec.MemoryID, rec.Hash, rec.SourceRunIDs, rec.EvidenceRefs)
}

func (s *MemoryStore) provenanceHMAC(memoryID, hash string, sourceRunIDs, evidenceRefs []string) string {
	mac := hmac.New(sha256.New, s.hmacKey)
	mac.Write([]byte(memoryID))
	mac.Write([]byte(hash))
	for _, id := range sourceRunIDs {
		mac.Write([]byte(id))
	}
	for _, ref := range evidenceRefs {
		mac.Write([]byte(ref))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func validateMemoryRecord(rec MemoryRecord) error {
	if rec.Content == "" {
		return fmt.Errorf("%w: content is required", ErrInvalidMemoryRecord)
	}
	if !oneOf(rec.Type, MemoryTypeEpisodic, MemoryTypeSemantic, MemoryTypeProcedural, MemoryTypeConstraint) {
		return fmt.Errorf("%w: invalid type %q", ErrInvalidMemoryRecord, rec.Type)
	}
	if !oneOf(rec.Scope, MemoryScopeSession, MemoryScopeUser, MemoryScopeProject, MemoryScopeTenant, MemoryScopeOrg) {
		return fmt.Errorf("%w: invalid scope %q", ErrInvalidMemoryRecord, rec.Scope)
	}
	if rec.Status == "" {
		return fmt.Errorf("%w: status is required", ErrInvalidMemoryRecord)
	}
	if !oneOf(rec.Status, MemoryStatusCandidate, MemoryStatusQuarantined, MemoryStatusValidated,
		MemoryStatusActive, MemoryStatusSuperseded, MemoryStatusRevoked) {
		return fmt.Errorf("%w: invalid status %q", ErrInvalidMemoryRecord, rec.Status)
	}
	if rec.TrustLevel != "" && !oneOf(rec.TrustLevel, TrustLevelUntrusted, TrustLevelCandidate, TrustLevelValidated, TrustLevelTrusted) {
		return fmt.Errorf("%w: invalid trust_level %q", ErrInvalidMemoryRecord, rec.TrustLevel)
	}
	if rec.Confidence < 0 || rec.Confidence > 1 {
		return fmt.Errorf("%w: confidence %v out of range [0,1]", ErrInvalidMemoryRecord, rec.Confidence)
	}
	if rec.UtilityScore < 0 {
		return fmt.Errorf("%w: utility_score %v must be >= 0", ErrInvalidMemoryRecord, rec.UtilityScore)
	}
	return nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanMemoryRow(row rowScanner) (*MemoryRecord, error) {
	var rec MemoryRecord
	var sourceRunIDsJSON, evidenceRefsJSON []byte
	var trustLevel *string
	err := row.Scan(
		&rec.MemoryID, &rec.Type, &rec.Scope, &rec.TenantID, &rec.Content, &sourceRunIDsJSON, &evidenceRefsJSON,
		&trustLevel, &rec.Confidence, &rec.UtilityScore, &rec.Status, &rec.ExpiresAt, &rec.Version, &rec.Hash,
		&rec.ProvenanceHMAC, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: scan memory record: %w", err)
	}
	if trustLevel != nil {
		rec.TrustLevel = *trustLevel
	}
	if err := json.Unmarshal(sourceRunIDsJSON, &rec.SourceRunIDs); err != nil {
		return nil, fmt.Errorf("store: unmarshal source_run_ids: %w", err)
	}
	if err := json.Unmarshal(evidenceRefsJSON, &rec.EvidenceRefs); err != nil {
		return nil, fmt.Errorf("store: unmarshal evidence_refs: %w", err)
	}
	return &rec, nil
}
