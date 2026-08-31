-- Aeon control-plane schema (FND-001 Agent Registry, TOOL-001 Tool Registry).
-- Applied idempotently at controlplane startup (see postgres.go Migrate). A real migration
-- framework (golang-migrate or similar) is TODO if/when this schema needs versioned migrations;
-- for now CREATE TABLE IF NOT EXISTS is sufficient since the schema has no history yet.

CREATE TABLE IF NOT EXISTS agents (
    name            TEXT NOT NULL,
    version         TEXT NOT NULL,
    owner           TEXT NOT NULL DEFAULT '',
    lifecycle       TEXT NOT NULL DEFAULT 'Draft',
    manifest        JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (name, version),
    CONSTRAINT agents_lifecycle_valid CHECK (lifecycle IN ('Draft', 'Candidate', 'Released', 'Retired'))
);

CREATE TABLE IF NOT EXISTS tools (
    tool_id             TEXT NOT NULL,
    version             TEXT NOT NULL,
    name                TEXT NOT NULL,
    side_effect         TEXT NOT NULL,
    risk                TEXT NOT NULL,
    descriptor          JSONB NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tool_id, version),
    CONSTRAINT tools_side_effect_valid CHECK (side_effect IN ('NONE', 'READ_ONLY', 'WRITE_REVERSIBLE', 'WRITE_IRREVERSIBLE')),
    CONSTRAINT tools_risk_valid CHECK (risk IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL'))
);

-- MEM-001 Memory Store: mirrors proto/schemas/memory_record.schema.json. hash and
-- provenance_hmac are always computed by MemoryStore itself (never trusted from a caller) —
-- the DB just persists them. status starts at CANDIDATE/QUARANTINED only; the
-- quarantine->validate->promote/reject state machine that reaches ACTIVE is MEM-002.
CREATE TABLE IF NOT EXISTS memory_records (
    memory_id       UUID NOT NULL PRIMARY KEY,
    type            TEXT NOT NULL,
    scope           TEXT NOT NULL,
    tenant_id       TEXT NOT NULL DEFAULT '',
    content         TEXT NOT NULL,
    source_run_ids  JSONB NOT NULL DEFAULT '[]',
    evidence_refs   JSONB NOT NULL DEFAULT '[]',
    trust_level     TEXT,
    confidence      DOUBLE PRECISION NOT NULL DEFAULT 0,
    utility_score   DOUBLE PRECISION NOT NULL DEFAULT 0,
    status          TEXT NOT NULL,
    expires_at      TIMESTAMPTZ,
    version         INTEGER NOT NULL DEFAULT 1,
    hash            TEXT NOT NULL,
    provenance_hmac TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT memory_records_type_valid CHECK (type IN ('EPISODIC', 'SEMANTIC', 'PROCEDURAL', 'CONSTRAINT')),
    CONSTRAINT memory_records_scope_valid CHECK (scope IN ('session', 'user', 'project', 'tenant', 'org')),
    CONSTRAINT memory_records_status_valid CHECK (status IN ('CANDIDATE', 'QUARANTINED', 'VALIDATED', 'ACTIVE', 'SUPERSEDED', 'REVOKED')),
    CONSTRAINT memory_records_trust_level_valid CHECK (trust_level IS NULL OR trust_level IN ('UNTRUSTED', 'CANDIDATE', 'VALIDATED', 'TRUSTED')),
    CONSTRAINT memory_records_confidence_range CHECK (confidence >= 0 AND confidence <= 1)
);

CREATE INDEX IF NOT EXISTS memory_records_scope_status_idx ON memory_records (scope, tenant_id, status);
