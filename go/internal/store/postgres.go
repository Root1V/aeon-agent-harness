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

// Migrate applies schema.sql. Idempotent: every statement is CREATE TABLE IF NOT EXISTS.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schemaSQL); err != nil {
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
