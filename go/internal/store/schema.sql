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
