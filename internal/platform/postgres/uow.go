package postgres

import (
	"context"
	"fmt"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UnitOfWork implements domain.UnitOfWork over the pool. It is the only place
// a transaction is opened for business work, and it owns the retry policy, so
// no use case can accidentally open an unretried or over-retrying transaction.
type UnitOfWork struct {
	pool   *pgxpool.Pool
	policy RetryPolicy
	sleep  Sleep
}

func NewUnitOfWork(pool *pgxpool.Pool, policy RetryPolicy, sleep Sleep) *UnitOfWork {
	return &UnitOfWork{pool: pool, policy: policy, sleep: sleep}
}

// Do runs fn inside one READ COMMITTED transaction, retrying serialization
// failures and deadlocks. The repositories handed to fn are bound to that
// transaction, so a claim, the ledger entries, the balance update, the outbox
// row and the audit row either all commit or all disappear.
func (u *UnitOfWork) Do(ctx context.Context, fn func(context.Context, domain.Tx) error) error {
	return WithTx(ctx, u.pool, u.policy, u.sleep,
		func(ctx context.Context, tx pgx.Tx) error {
			return fn(ctx, domain.Tx{
				Ledger:     NewLedgerRepo(tx),
				Operations: NewOperationsRepo(tx),
				Audit:      NewAuditRepo(tx),
				Outbox:     NewOutboxRepo(tx),
			})
		})
}

// Pool exposes the underlying pool for read-only repositories and health
// checks. It is not a business write path.
func (u *UnitOfWork) Pool() *pgxpool.Pool { return u.pool }

// HealthCheck verifies the database is reachable. It backs /health/ready.
func HealthCheck(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return fmt.Errorf("no database pool configured")
	}
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	return nil
}
