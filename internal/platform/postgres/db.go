// Package postgres holds the PostgreSQL adapters. It is the only package that
// knows the SQL; the domain declares the interfaces these types satisfy.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig carries the tunables the plan's performance targets depend on.
type PoolConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	// ConnectTimeout bounds a single connection attempt.
	ConnectTimeout time.Duration
	// StatementTimeout bounds one statement, so a pathological statement cannot
	// hold a wallet row lock indefinitely.
	StatementTimeout time.Duration
	// ApplicationName appears in pg_stat_activity, which is how an operator
	// finds the statement holding a lock.
	ApplicationName string
}

// DefaultPoolConfig returns the settings used by the service and the tests.
// MaxConns is deliberately well above the expected concurrent wallet count: the
// lock is per wallet row, so connections are not a serialization point.
func DefaultPoolConfig(url string) PoolConfig {
	return PoolConfig{
		URL:              url,
		MaxConns:         32,
		MinConns:         2,
		MaxConnLifetime:  time.Hour,
		MaxConnIdleTime:  30 * time.Minute,
		ConnectTimeout:   10 * time.Second,
		StatementTimeout: 5 * time.Second,
		ApplicationName:  "ironledger",
	}
}

// Connect opens a pool and verifies it can reach the server.
func Connect(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	if cfg.ApplicationName != "" {
		poolCfg.ConnConfig.RuntimeParams["application_name"] = cfg.ApplicationName
	}
	if cfg.StatementTimeout > 0 {
		poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = formatMillis(cfg.StatementTimeout)
	}
	// READ COMMITTED is the service's isolation level: with SELECT ... FOR
	// UPDATE on the wallet row it already gives read-modify-write safety, and
	// it avoids the retry storms SERIALIZABLE produces for a single-row
	// contention point (research.md D-7). It is set per transaction in tx.go.
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

func formatMillis(d time.Duration) string {
	ms := d.Milliseconds()
	if ms <= 0 {
		ms = 1
	}
	return fmt.Sprintf("%d", ms)
}
