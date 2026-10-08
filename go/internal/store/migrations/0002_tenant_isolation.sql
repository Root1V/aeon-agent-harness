-- 0002 · Tenant isolation for every table a consumer can reach (VRT-AEON-005, T-2/T-3/T-4).
--
-- THE FIRST DESTRUCTIVE MIGRATION, and the reason GOV-002b had to exist first. Six of the eight
-- changes below alter a PRIMARY KEY or a UNIQUE constraint, which `CREATE TABLE IF NOT EXISTS`
-- cannot express: it skips an existing table in silence. A deployment would have believed
-- deduplication was tenant-scoped, and found out when one tenant's step was treated as already
-- executed because another used the same idempotency key.
--
-- EXISTING ROWS GO TO 'default', which is Veritium's acceptance criterion 5 ("los datos existentes
-- pasan a un tenant `default` sin pérdida") and what the shipped caller bundles declare. The column
-- is added with that DEFAULT and NOT NULL so the backfill is the ALTER itself rather than an UPDATE
-- that could be interrupted half way.
--
-- `memory_records` already had a tenant_id (SEC-004) with DEFAULT '', so rows written before
-- AEON_TENANT_ID was threaded through carry the empty string. Those are migrated to 'default' too:
-- leaving them would put real data in a tenant no caller can name, which is loss by another route.
--
-- NOT TENANT-SCOPED, on purpose and agreed with Veritium: `model_quality_scores`. A model's eval
-- score is a fact about the MODEL, not about a tenant. Splitting it would make every tenant
-- re-measure the same model and make MDL-002's routing threshold mean different things per tenant
-- for no reason. It is not in their T-4 list either.

-- ---------------------------------------------------------------------------------------------
-- T-2 · Costs. Additive: the primary key is a BIGSERIAL, so only the column and an index change.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE model_gateway_costs ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
CREATE INDEX IF NOT EXISTS model_gateway_costs_tenant_idx ON model_gateway_costs (tenant_id);

-- ---------------------------------------------------------------------------------------------
-- T-3 · Deduplication. THE ONE WITH TEETH: the idempotency key was the whole primary key, so the
-- same key in two tenants was one row, and the second tenant's call came back "already executed"
-- with the first tenant's recorded result.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE tool_executions ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE tool_executions DROP CONSTRAINT IF EXISTS tool_executions_pkey;
ALTER TABLE tool_executions ADD PRIMARY KEY (tenant_id, idempotency_key);

-- ---------------------------------------------------------------------------------------------
-- T-3 · Checkpoints. Both constraints move: the key AND the per-run sequence, because a sequence
-- scoped to a run id that two tenants can both use is a sequence two runs share.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE run_checkpoints ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE run_checkpoints DROP CONSTRAINT IF EXISTS run_checkpoints_pkey;
ALTER TABLE run_checkpoints ADD PRIMARY KEY (tenant_id, run_id, step_id, phase);
ALTER TABLE run_checkpoints DROP CONSTRAINT IF EXISTS run_checkpoints_seq_unique;
ALTER TABLE run_checkpoints ADD CONSTRAINT run_checkpoints_seq_unique UNIQUE (tenant_id, run_id, seq);

-- ---------------------------------------------------------------------------------------------
-- T-4 · Registries. Two tenants may use the same agent or tool name without colliding, and
-- without either being able to see the other's.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE agents ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE agents DROP CONSTRAINT IF EXISTS agents_pkey;
ALTER TABLE agents ADD PRIMARY KEY (tenant_id, name, version);

ALTER TABLE tools ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE tools DROP CONSTRAINT IF EXISTS tools_pkey;
ALTER TABLE tools ADD PRIMARY KEY (tenant_id, tool_id, version);

ALTER TABLE remote_agents ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE remote_agents DROP CONSTRAINT IF EXISTS remote_agents_pkey;
ALTER TABLE remote_agents ADD PRIMARY KEY (tenant_id, agent_id);

ALTER TABLE a2a_delegations ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
CREATE INDEX IF NOT EXISTS a2a_delegations_tenant_idx ON a2a_delegations (tenant_id);

ALTER TABLE rag_chunks ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE rag_chunks DROP CONSTRAINT IF EXISTS rag_chunks_unique_locator;
ALTER TABLE rag_chunks ADD CONSTRAINT rag_chunks_unique_locator UNIQUE (tenant_id, corpus, source_path, chunk_index);

-- ---------------------------------------------------------------------------------------------
-- Criterion 5 · the rows that predate AEON_TENANT_ID being threaded through.
-- ---------------------------------------------------------------------------------------------
UPDATE memory_records SET tenant_id = 'default' WHERE tenant_id = '';
ALTER TABLE memory_records ALTER COLUMN tenant_id SET DEFAULT 'default';
