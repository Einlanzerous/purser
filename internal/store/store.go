// Package store wires Purser to its Postgres backend: connection pooling, a
// small embedded migrator (mirrors the construct-server house pattern — no
// external migration tool), and the query methods the invite orchestrator uses.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the Postgres-backed persistence layer.
type Store struct {
	pool *pgxpool.Pool
}

// Connect opens a pgx connection pool for the given DSN and verifies it is
// reachable with a Ping. The caller owns the pool; New takes it over.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: new pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return pool, nil
}

// New builds a Store over an already-connected pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the underlying pool for callers that need direct access (tests).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }
