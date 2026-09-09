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

-- MEM-005 (Utility/Forgetting): additive migration. CREATE TABLE IF NOT EXISTS above is a no-op
-- against a memory_records table that already exists from an earlier deploy, so these new columns
-- need their own idempotent ALTER — the first real schema history this table has had.
ALTER TABLE memory_records ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ;
ALTER TABLE memory_records ADD COLUMN IF NOT EXISTS superseded_by UUID REFERENCES memory_records(memory_id);

-- A5 (circuit breaker + kill switch): additive migration, same reasoning as MEM-005 above. This is
-- a real, orthogonal flag alongside `lifecycle` — a Released agent that gets quarantined does NOT
-- change lifecycle (still "Released"), since Quarantine/Unquarantine are not part of the forward-only
-- Draft->Candidate->Released->Retired path TransitionLifecycle enforces (see AgentRegistry.Quarantine).
ALTER TABLE agents ADD COLUMN IF NOT EXISTS quarantined BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS quarantine_reason TEXT;

-- OBS-003 (FinOps): a durable ledger of real model-gateway cost events. run_id/agent_manifest_ref
-- are nullable — no real caller populates them yet (see backlog.md), so a row logged today has cost
-- attributable to a provider/model, not yet to a specific run or agent.
CREATE TABLE IF NOT EXISTS model_gateway_costs (
    id                  BIGSERIAL PRIMARY KEY,
    provider            TEXT NOT NULL,
    model               TEXT NOT NULL,
    cost_model          TEXT NOT NULL,
    prompt_tokens       INTEGER NOT NULL DEFAULT 0,
    completion_tokens   INTEGER NOT NULL DEFAULT 0,
    cost_usd            DOUBLE PRECISION NOT NULL DEFAULT 0,
    run_id              TEXT,
    agent_manifest_ref  TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS model_gateway_costs_model_idx ON model_gateway_costs (provider, model);
CREATE INDEX IF NOT EXISTS model_gateway_costs_run_idx ON model_gateway_costs (run_id);

-- MDL-002 (quality-aware routing): the current real eval score per (provider, model) — a real
-- eval suite (e.g. provider_conformance) reports here; the Model Gateway consults it to skip a
-- degraded candidate before ever attempting it. One row per (provider, model) — the latest report
-- replaces the previous one; no history is kept here (see backlog.md).
CREATE TABLE IF NOT EXISTS model_quality_scores (
    provider    TEXT NOT NULL,
    model       TEXT NOT NULL,
    score       DOUBLE PRECISION NOT NULL,
    suite       TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, model)
);

-- INT-009 (durability seam): the append-only run journal the Synaptum framework writes through.
-- The PRIMARY KEY is the idempotency contract, not a convenience index: (run_id, step_id, phase)
-- is exactly the identity under which a duplicate append must be a no-op, and Postgres enforcing it
-- means a caller that skips Checkpointer.Append's own duplicate check still cannot double-write.
-- seq is contiguous per run (assigned under a per-run advisory lock — see store.Checkpointer),
-- which is what makes RunState.NextSeq mean "where the journal continues" rather than "some number
-- larger than the last one".
CREATE TABLE IF NOT EXISTS run_checkpoints (
    run_id      TEXT NOT NULL,
    step_id     TEXT NOT NULL,
    phase       TEXT NOT NULL,
    seq         BIGINT NOT NULL,
    payload     JSONB,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, step_id, phase),
    CONSTRAINT run_checkpoints_phase_valid CHECK (phase IN ('attempted', 'completed')),
    CONSTRAINT run_checkpoints_seq_unique UNIQUE (run_id, seq)
);
CREATE INDEX IF NOT EXISTS run_checkpoints_run_seq_idx ON run_checkpoints (run_id, seq);
