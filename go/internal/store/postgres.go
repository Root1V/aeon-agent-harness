// Package store is the control plane's persistence layer: the Agent Registry (FND-001) and Tool
// Registry (TOOL-001) tables, backed by Postgres. See schema.sql for the DDL, embedded here so
// the binary carries its own schema and Migrate() can apply it idempotently at startup.
package store

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

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
// each package's tests as a separate process against the same real Postgres) can run schema.sql's
// DDL — CREATE TABLE and, since MEM-005, ALTER TABLE ADD COLUMN with a self-referencing FOREIGN
// KEY — at the same time and deadlock (observed directly: SQLSTATE 40P01 from concurrent Migrate
// calls). The lock makes every Connect() apply schema.sql one at a time instead.
const migrationLockID = 727272727001

// Migrate applies schema.sql. Idempotent (CREATE TABLE/ADD COLUMN IF NOT EXISTS throughout) and
// safe under concurrent callers — see migrationLockID.
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

	if _, err := conn.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// AgentRegistry returns a registry handle for agents (FND-001).
func (s *Store) AgentRegistry() *AgentRegistry {
	return &AgentRegistry{pool: s.pool}
}

// ToolRegistry returns a registry handle for tools (TOOL-001).
func (s *Store) ToolRegistry() *ToolRegistry {
	return &ToolRegistry{pool: s.pool}
}

// FinOpsLedger returns a handle for the real model-gateway cost ledger (OBS-003).
func (s *Store) FinOpsLedger() *FinOpsLedger {
	return &FinOpsLedger{pool: s.pool}
}

// QualityScores returns a handle for MDL-002's quality score store. threshold is the score below
// which IsDegraded reports true (e.g. 0.9 for "degraded once below 90% pass rate").
func (s *Store) QualityScores(threshold float64) *QualityScoreStore {
	return &QualityScoreStore{pool: s.pool, threshold: threshold}
}

// Checkpointer returns a handle for INT-009's durability seam — the run journal the Synaptum
// framework appends to. See go/internal/checkpoint.
func (s *Store) Checkpointer() *Checkpointer {
	return &Checkpointer{pool: s.pool}
}
