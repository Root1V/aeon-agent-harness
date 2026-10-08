// Package store is the control plane's persistence layer: the Agent Registry (FND-001) and Tool
// Registry (TOOL-001) tables, backed by Postgres. The DDL lives in migrations/, embedded so the
// binary carries its own schema and Migrate() brings a database up to the version it knows.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps a Postgres connection pool and exposes the registries built on top of it.
type Store struct {
	pool *pgxpool.Pool
}

// Connect opens a pool against dsn (e.g. AEON_PG_DSN) and applies the schema.
func Connect(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// migrationLockID is a fixed Postgres advisory lock key serializing schema application. Without
// it, many processes calling Connect() concurrently (routine under `go test ./...`, which runs
// each package's tests as a separate process against the same real Postgres) can run the migrations's
// DDL — CREATE TABLE and, since MEM-005, ALTER TABLE ADD COLUMN with a self-referencing FOREIGN
// KEY — at the same time and deadlock (observed directly: SQLSTATE 40P01 from concurrent Migrate
// calls). The lock makes every Connect() apply the migrations one at a time instead.
const migrationLockID = 727272727001

// Migrate brings the database up to the migration version this binary carries (migrations.go).
//
// Idempotent in the sense that now matters: an already-applied migration is skipped because the
// ledger says it ran, not because the DDL happens to be a no-op when repeated. That difference is
// the whole point of the change — a destructive migration is not a no-op when repeated, and the old
// mechanism had no way to know it had already run. Safe under concurrent callers; see
// migrationLockID.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire connection for migration: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(migrationLockID)); err != nil {
		return fmt.Errorf("store: acquire migration lock: %w", err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(migrationLockID))

	// VRT-AEON-005: ordered, recorded migrations instead of one idempotent file. Loaded and
	// validated BEFORE touching the database, so a malformed set (a gap, a duplicate version, a file
	// that is not named NNNN_name.sql) stops the service rather than applying a partial history.
	pending, err := loadMigrations()
	if err != nil {
		return err
	}
	if err := applyMigrations(ctx, conn, pending); err != nil {
		return err
	}
	return nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// AgentRegistry returns a registry handle for agents (FND-001).
// AgentRegistryFor returns the handle SCOPED TO ONE TENANT (VRT-AEON-005 T-4).
//
// The tenant is a CONSTRUCTOR argument, not a method parameter, and that is the seam: slice 2 found
// the Memory Store's isolation implemented route by route — four routes read the tenant from the
// request and five checked none at all — because every method was a separate chance to forget. A
// handle that cannot exist without a tenant makes the compiler answer that once, here.
func (s *Store) AgentRegistryFor(tenant string) *AgentRegistry {
	return &AgentRegistry{pool: s.pool, tenant: tenant}
}

// ToolRegistry returns a registry handle for tools (TOOL-001).
// ToolRegistryFor returns the handle SCOPED TO ONE TENANT (VRT-AEON-005 T-4).
//
// The tenant is a CONSTRUCTOR argument, not a method parameter, and that is the seam: slice 2 found
// the Memory Store's isolation implemented route by route — four routes read the tenant from the
// request and five checked none at all — because every method was a separate chance to forget. A
// handle that cannot exist without a tenant makes the compiler answer that once, here.
func (s *Store) ToolRegistryFor(tenant string) *ToolRegistry {
	return &ToolRegistry{pool: s.pool, tenant: tenant}
}

// FinOpsLedger returns a handle for the real model-gateway cost ledger (OBS-003).
// FinOpsLedgerFor returns the handle SCOPED TO ONE TENANT (VRT-AEON-005 T-2).
//
// The tenant is a CONSTRUCTOR argument, not a method parameter, and that is the seam: slice 2 found
// the Memory Store's isolation implemented route by route — four routes read the tenant from the
// request and five checked none at all — because every method was a separate chance to forget. A
// handle that cannot exist without a tenant makes the compiler answer that once, here.
func (s *Store) FinOpsLedgerFor(tenant string) *FinOpsLedger {
	return &FinOpsLedger{pool: s.pool, tenant: tenant}
}

// QualityScores returns a handle for MDL-002's quality score store. threshold is the score below
// which IsDegraded reports true (e.g. 0.9 for "degraded once below 90% pass rate").
func (s *Store) QualityScores(threshold float64) *QualityScoreStore {
	return &QualityScoreStore{pool: s.pool, threshold: threshold}
}

// Checkpointer returns a handle for INT-009's durability seam — the run journal the Synaptum
// framework appends to. See go/internal/checkpoint.
// CheckpointerFor returns the handle SCOPED TO ONE TENANT (VRT-AEON-005 T-3).
//
// The tenant is a CONSTRUCTOR argument, not a method parameter, and that is the seam: slice 2 found
// the Memory Store's isolation implemented route by route — four routes read the tenant from the
// request and five checked none at all — because every method was a separate chance to forget. A
// handle that cannot exist without a tenant makes the compiler answer that once, here.
func (s *Store) CheckpointerFor(tenant string) *Checkpointer {
	return &Checkpointer{pool: s.pool, tenant: tenant}
}
