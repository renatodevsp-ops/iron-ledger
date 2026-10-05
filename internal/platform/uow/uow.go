// Package uow provides the transaction boundary every business operation runs
// inside.
//
// One iron-ledger operation — register a wager transaction, move the wallet,
// write the ledger, record the inbox message, enqueue outbound events — is one
// PostgreSQL transaction. That is what makes "no duplicated movement, no lost
// update, no event published before its commit" a property of the database
// rather than of a distributed lock nobody can reason about.
//
// Coordination between concurrent writers is per wallet, never global:
//
//  1. The manager takes a transaction-scoped advisory lock derived from the
//     wallet id before any read, so writers to *different* wallets never wait
//     for each other.
//  2. Every projection write additionally carries the wallet version it was
//     derived from (optimistic check), and every financial invariant is
//     re-asserted by a database constraint.
//  3. A writer that still loses the race retries the whole transaction from
//     the beginning, bounded by MaxConflictRetries.
package uow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/platform/logging"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
)

// ErrConcurrent marks a lost-update race. It is retryable: the caller should
// run the whole transaction again, not surface it to the caller.
var ErrConcurrent = errors.New("uow: concurrent modification")

// pgRetryable are the SQLSTATE classes worth retrying as a whole transaction.
const (
	sqlStateSerializationFailure = "40001"
	sqlStateDeadlockDetected     = "40P01"
)

// Options configures one transaction.
type Options struct {
	// Name identifies the operation in logs and metrics.
	Name string
	// WalletID, when set, serialises the transaction against every other
	// writer touching the same wallet for the duration of the transaction.
	WalletID string
	// MaxRetries overrides the manager default for this operation.
	MaxRetries int
	// NoRetry disables the retry loop, for read-only transactions.
	NoRetry bool
}

type ctxKey int

const (
	txKey ctxKey = iota
	recorderKey
)

// txState is the mutable state of an in-flight transaction.
type txState struct {
	tx       pgx.Tx
	envelope *EnvelopeRecorder
}

// EnvelopeRecorder collects every envelope appended during a transaction, so
// the caller can project them into read models and enqueue outbound events
// inside the very same commit.
type EnvelopeRecorder struct {
	events []cqrs.Envelope
}

// Record adds an envelope to the transaction's append log.
func (r *EnvelopeRecorder) Record(envelopes ...cqrs.Envelope) {
	if r == nil {
		return
	}
	r.events = append(r.events, envelopes...)
}

// Envelopes returns the envelopes appended so far, in order.
func (r *EnvelopeRecorder) Envelopes() []cqrs.Envelope {
	if r == nil {
		return nil
	}
	return r.events
}

// Drain returns the envelopes appended so far and clears the buffer, so a
// projection is applied exactly once per envelope even when the transaction
// flushes more than once.
func (r *EnvelopeRecorder) Drain() []cqrs.Envelope {
	if r == nil || len(r.events) == 0 {
		return nil
	}
	drained := r.events
	r.events = nil
	return drained
}

// Manager runs transactions.
type Manager struct {
	pool               *pgxpool.Pool
	logger             *slog.Logger
	metrics            *metrics.Metrics
	maxConflictRetries int
	baseBackoff        time.Duration
}

// NewManager builds a Manager over a pool.
func NewManager(pool *pgxpool.Pool, logger *slog.Logger, m *metrics.Metrics, maxConflictRetries int) *Manager {
	if maxConflictRetries < 1 {
		maxConflictRetries = 1
	}
	return &Manager{
		pool:               pool,
		logger:             logger,
		metrics:            m,
		maxConflictRetries: maxConflictRetries,
		baseBackoff:        2 * time.Millisecond,
	}
}

// Do runs fn inside a transaction, retrying the whole transaction while it
// keeps losing optimistic-concurrency races.
//
// fn must be free of side effects outside the database: it may run more than
// once. Every repository and the event store reached through the ctx it is
// given participate in the same transaction.
func (m *Manager) Do(ctx context.Context, opts Options, fn func(ctx context.Context) error) error {
	if InTx(ctx) {
		return fmt.Errorf("uow: nested transaction requested for %q", opts.Name)
	}

	maxRetries := m.maxConflictRetries
	if opts.MaxRetries > 0 {
		maxRetries = opts.MaxRetries
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := m.runOnce(ctx, opts, fn)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrConcurrent) || opts.NoRetry {
			return err
		}

		lastErr = err
		if m.metrics != nil {
			m.metrics.ConcurrencyConflicts.WithLabelValues("retry").Inc()
		}
		if attempt == maxRetries-1 {
			break
		}

		delay := m.baseBackoff << attempt
		if delay > 250*time.Millisecond {
			delay = 250 * time.Millisecond
		}
		if m.logger != nil {
			m.logger.Debug("retrying transaction after concurrency conflict",
				"operation", opts.Name,
				"attempt", attempt+1,
				"maxRetries", maxRetries,
				"error", err.Error(),
			)
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}

	if m.metrics != nil {
		m.metrics.ConcurrencyConflicts.WithLabelValues("exhausted").Inc()
	}
	return fmt.Errorf("uow: %q gave up after %d conflicting attempts: %w", opts.Name, maxRetries, lastErr)
}

func (m *Manager) runOnce(ctx context.Context, opts Options, fn func(ctx context.Context) error) error {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("uow: begin %q: %w", opts.Name, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	state := &txState{tx: tx, envelope: &EnvelopeRecorder{}}
	txCtx := context.WithValue(ctx, txKey, state)

	// Serialise writers of the same wallet before reading anything, so the
	// first statement of this transaction already observes the winner's
	// commit. Different wallets take different locks and never contend.
	if opts.WalletID != "" {
		const lockWallet = `SELECT pg_advisory_xact_lock(hashtext($1))`
		if _, err := tx.Exec(txCtx, lockWallet, "ironledger:wallet:"+opts.WalletID); err != nil {
			return m.wrap(ctx, opts, fmt.Errorf("lock wallet: %w", err))
		}
	}

	if err := fn(txCtx); err != nil {
		return m.wrap(ctx, opts, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return m.wrap(ctx, opts, fmt.Errorf("commit: %w", err))
	}
	committed = true
	return nil
}

func (m *Manager) wrap(ctx context.Context, opts Options, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if isRetryable(err) {
		return fmt.Errorf("uow: %q: %w: %v", opts.Name, ErrConcurrent, err)
	}
	return fmt.Errorf("uow: %q: %w", opts.Name, err)
}

// isRetryable classifies the errors that mean "another writer got there first"
// rather than "this operation is invalid".
func isRetryable(err error) bool {
	var conflict *cqrs.StreamRevisionConflictError
	if errors.As(err, &conflict) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case sqlStateSerializationFailure, sqlStateDeadlockDetected:
			return true
		}
	}
	return false
}

// InTx reports whether ctx already carries a transaction.
func InTx(ctx context.Context) bool {
	state, _ := ctx.Value(txKey).(*txState)
	return state != nil && state.tx != nil
}

// TxFromContext returns the transaction carried by ctx.
func TxFromContext(ctx context.Context) (pgx.Tx, error) {
	state, _ := ctx.Value(txKey).(*txState)
	if state == nil || state.tx == nil {
		return nil, errors.New("uow: no transaction in context")
	}
	return state.tx, nil
}

// RecorderFromContext returns the envelope recorder carried by ctx, or nil when
// ctx carries no transaction.
func RecorderFromContext(ctx context.Context) *EnvelopeRecorder {
	state, _ := ctx.Value(txKey).(*txState)
	if state == nil {
		return nil
	}
	return state.envelope
}

// WithRecorder returns a context carrying an explicit recorder. It is used by
// tests and by the event store to record envelopes outside a UoW.
func WithRecorder(ctx context.Context, r *EnvelopeRecorder) context.Context {
	return context.WithValue(ctx, recorderKey, r)
}

// LogFields renders the correlation identifiers of ctx for structured logs.
func LogFields(ctx context.Context) []any {
	fields := make([]any, 0, 8)
	if v := logging.CorrelationID(ctx); v != "" {
		fields = append(fields, logging.FieldCorrelationID, v)
	}
	if v := logging.MessageID(ctx); v != "" {
		fields = append(fields, logging.FieldMessageID, v)
	}
	if v := logging.TransactionID(ctx); v != "" {
		fields = append(fields, logging.FieldTransactionID, v)
	}
	if v := logging.WalletID(ctx); v != "" {
		fields = append(fields, logging.FieldWalletID, v)
	}
	if v := logging.ProviderID(ctx); v != "" {
		fields = append(fields, logging.FieldProviderID, v)
	}
	return fields
}
