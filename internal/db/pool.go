package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// errInvalidDSN is deliberately opaque. pgx parse errors echo the input DSN,
// which can include a password, and these errors are printed at process
// startup.
var errInvalidDSN = errors.New("invalid dsn syntax")

// parsePoolConfig parses dsn without retaining pgx's cause. Callers add a role
// or pool name but never the DSN itself.
func parsePoolConfig(dsn string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errInvalidDSN
	}
	return cfg, nil
}

// NewPool parses dsn, opens a pgx pool, and verifies connectivity with a ping.
// The caller owns the returned pool and must close it when done.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := parsePoolConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}
