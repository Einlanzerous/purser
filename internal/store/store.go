// Package store wires Purser to its Postgres backend: connection pooling, a
// small embedded migrator (mirrors the construct-server house pattern — no
// external migration tool), and the query methods the invite orchestrator uses.
package store

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

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

// ConnectWithRetry opens the pool, retrying transient failures with exponential
// backoff up to maxWait before giving up. A Postgres restart (e.g. recreating
// the shared container) is thus ridden out instead of crash-looping the process
// via the boot-time fatal. A genuinely bad DSN still fails — just after the
// budget, with a clear message.
//
// It also fails LOUDLY on an obviously-broken credential: an empty password in
// the DSN (a blank ${PURSER_DB_PASSWORD}) otherwise surfaces only as an opaque
// SASL error, which was the root cause of two real crash-loops.
func ConnectWithRetry(ctx context.Context, dsn string, maxWait time.Duration) (*pgxpool.Pool, error) {
	if cfg, err := pgxpool.ParseConfig(dsn); err == nil {
		if cfg.ConnConfig.Password == "" && os.Getenv("PGPASSWORD") == "" {
			log.Printf("store: WARNING — database password is EMPTY (DSN carries no password and PGPASSWORD is unset); check ${PURSER_DB_PASSWORD} interpolation")
		}
	}

	deadline := time.Now().Add(maxWait)
	delay := 500 * time.Millisecond
	const maxDelay = 5 * time.Second
	var lastErr error

	for attempt := 1; ; attempt++ {
		pool, err := Connect(ctx, dsn)
		if err == nil {
			if attempt > 1 {
				log.Printf("store: connected after %d attempt(s)", attempt)
			}
			return pool, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("store: giving up after %s: %w", maxWait, lastErr)
		}
		log.Printf("store: connect attempt %d failed (%v); retrying in %s", attempt, err, delay)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay *= 2; delay > maxDelay {
			delay = maxDelay
		}
	}
}

// New builds a Store over an already-connected pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the underlying pool for callers that need direct access (tests).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }
