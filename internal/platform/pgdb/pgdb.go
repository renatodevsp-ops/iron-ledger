// Package pgdb owns the PostgreSQL pool and the plumbing every repository
// needs: resolving the transaction carried by a context, and probing the
// database for readiness.
package pgdb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ironledger/iron-ledger/internal/platform/config"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
)

// Querier is the read/write surface shared by a pool and an in-flight
// transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Pool builds and validates the connection pool.
type Pool struct{ *pgxpool.Pool }

// NewPool builds a pool from the configuration, verifying that the database
// answers before the process reports itself ready.
func NewPool(ctx context.Context, cfg config.Database) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("pgdb: parse DATABASE_URL: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime

	if poolCfg.ConnConfig.ConnectTimeout > 0 && cfg.ConnectTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("pgdb: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout+2*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgdb: ping: %w", err)
	}
	return pool, nil
}

// Resolver hands out the Querier appropriate for a context: the transaction it
// carries when there is one, the pool otherwise.
type Resolver struct{ pool *pgxpool.Pool }

// NewResolver builds a Resolver over a pool.
func NewResolver(pool *pgxpool.Pool) Resolver { return Resolver{pool: pool} }

// Q returns the Querier for ctx.
func (r Resolver) Q(ctx context.Context) Querier {
	if tx, err := uow.TxFromContext(ctx); err == nil {
		return tx
	}
	return r.pool
}

// Pool exposes the underlying pool for lifecycle management.
func (r Resolver) Pool() *pgxpool.Pool { return r.pool }

// Ping verifies the database answers a trivial query. It backs the readiness
// probe.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("pgdb: ping: %w", err)
	}
	return nil
}

// InTx reports whether ctx already carries a transaction.
func InTx(ctx context.Context) bool { return uow.InTx(ctx) }

// RedactDSN removes the credentials from a connection string so it can be
// logged without leaking a password.
func RedactDSN(dsn string) string {
	scheme := strings.Index(dsn, "://")
	if scheme < 0 {
		return "***"
	}
	rest := dsn[scheme+3:]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return dsn
	}
	return dsn[:scheme+3] + "***@" + rest[at+1:]
}

// SQLSTATE classes the application branches on.
const (
	SQLStateUniqueViolation     = "23505"
	SQLStateForeignKeyViolation = "23503"
	SQLStateCheckViolation      = "23514"
)

// Code extracts the SQLSTATE of a PostgreSQL error, if it is one.
func Code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// IsUniqueViolation reports whether err is a unique-constraint violation.
func IsUniqueViolation(err error) bool { return Code(err) == SQLStateUniqueViolation }

// IsForeignKeyViolation reports whether err is a foreign-key violation.
func IsForeignKeyViolation(err error) bool { return Code(err) == SQLStateForeignKeyViolation }

// ConstraintName returns the name of the constraint a PostgreSQL error refers
// to, or "".
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}
