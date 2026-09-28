package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SQLSTATEs that mean "retry the whole transaction" and nothing else.
const (
	sqlstateSerializationFailure = "40001"
	sqlstateDeadlockDetected     = "40P01"
	sqlstateLockNotAvailable     = "55P03"
)

// sqlstatesNeverRetried are business-rule rejections that must surface to the
// caller. Retrying them would only delay the error and, for a unique violation
// on an idempotency claim, hide a real conflict (research.md D-7).
var sqlstatesNeverRetried = map[string]bool{
	"23505": true, // unique_violation
	"23514": true, // check_violation
	"23503": true, // foreign_key_violation
	"23502": true, // not_null_violation
	"23001": true, // restrict_violation, raised by the append-only trigger
}

// RetryPolicy is the bounded retry used for transient database failures.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// DefaultRetryPolicy is 5 attempts with exponential backoff and full jitter,
// which is what the constitution and research.md D-2/D-7 agree on.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 5, BaseDelay: 2 * time.Millisecond, MaxDelay: 100 * time.Millisecond}
}

// Sleep is overridable so a test can exercise the retry path without wall-clock
// delays.
type Sleep func(ctx context.Context, d time.Duration) error

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// IsRetryable reports whether err is a transient database failure worth a whole
// transaction retry.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if sqlstatesNeverRetried[pgErr.Code] {
		return false
	}
	switch pgErr.Code {
	case sqlstateSerializationFailure, sqlstateDeadlockDetected, sqlstateLockNotAvailable:
		return true
	default:
		return false
	}
}

// WithTx runs fn inside one READ COMMITTED transaction, committing on success
// and rolling back on any error or panic. A serialization failure or deadlock
// is retried with exponential backoff and full jitter, up to MaxAttempts.
//
// The closure is re-run from the start on retry, so it must not carry state
// across attempts; everything it needs comes from the database.
func WithTx(ctx context.Context, pool *pgxpool.Pool, policy RetryPolicy, sleep Sleep, fn func(context.Context, pgx.Tx) error) error {
	if sleep == nil {
		sleep = defaultSleep
	}
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = runOnce(ctx, pool, fn)
		if lastErr == nil {
			return nil
		}
		if !IsRetryable(lastErr) || attempt == policy.MaxAttempts {
			return lastErr
		}
		if err := sleep(ctx, backoff(policy, attempt)); err != nil {
			return err
		}
	}
	return lastErr
}

func runOnce(ctx context.Context, pool *pgxpool.Pool, fn func(context.Context, pgx.Tx) error) (err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if err = fn(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// backoff returns the delay for the given 1-based attempt: exponential with
// full jitter, so N contending writers spread out instead of retrying in lock
// step.
func backoff(policy RetryPolicy, attempt int) time.Duration {
	max := policy.BaseDelay << (attempt - 1)
	if max > policy.MaxDelay || max <= 0 {
		max = policy.MaxDelay
	}
	// rand/v2 Int64N is not a financial computation; the retry jitter is not
	// money and never touches a monetary value.
	return time.Duration(rand.Int64N(int64(max)) + int64(policy.BaseDelay))
}
