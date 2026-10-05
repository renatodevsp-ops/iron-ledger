package wagertransactiondetails

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ironledger/iron-ledger/internal/platform/pgdb"
	"github.com/ironledger/iron-ledger/internal/platform/repository"
)

// Repo is the PostgreSQL implementation of Repository.
type Repo struct{ resolver pgdb.Resolver }

// NewRepo builds the repository.
func NewRepo(resolver pgdb.Resolver) *Repo { return &Repo{resolver: resolver} }

var _ Repository = (*Repo)(nil)

const columns = `id, origin, provider_id, external_transaction_id, idempotency_key, payload_hash,
	wallet_id, player_id, round_id, game_id, kind, amount_minor, currency,
	reference_external_id, reference_transaction_id, status, failure_code, failure_message,
	balance_after_minor, balance_after_currency, pending_attempts, next_attempt_at,
	created_at, updated_at`

// Find returns a transaction by its internal identity.
func (r *Repo) Find(ctx context.Context, id string) (*Entity, error) {
	transactionID, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("wagertransactiondetails: %q is not a transaction id: %w", id, repository.ErrNotFound)
	}
	return r.scanOne(ctx, "WHERE id = $1", transactionID)
}

// FindByIdempotencyKey returns the transaction a provider already sent under a key.
func (r *Repo) FindByIdempotencyKey(ctx context.Context, providerID, key string) (*Entity, error) {
	return r.scanOne(ctx, "WHERE provider_id = $1 AND idempotency_key = $2", providerID, key)
}

// FindByExternalID returns the transaction a provider already sent under its own
// identity.
func (r *Repo) FindByExternalID(ctx context.Context, providerID, externalID string) (*Entity, error) {
	return r.scanOne(ctx, "WHERE provider_id = $1 AND external_transaction_id = $2", providerID, externalID)
}

// FindReversalOf returns the successful reversal of a given operation.
func (r *Repo) FindReversalOf(ctx context.Context, referenceID uuid.UUID) (*Entity, error) {
	const query = `
		SELECT ` + columns + ` FROM ` + TableName + `
		 WHERE reference_transaction_id = $1
		   AND kind IN ('REFUND', 'ROLLBACK')
		   AND status = 'PROCESSED'
		 ORDER BY id ASC
		 LIMIT 1`
	entity := &Entity{}
	err := r.resolver.Q(ctx).QueryRow(ctx, query, referenceID).Scan(scanTargets(entity)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("wagertransactiondetails: no reversal: %w", repository.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("wagertransactiondetails: find reversal: %w", err)
	}
	return entity, nil
}

// DuePending returns the reversals waiting for a reference whose next attempt is
// due. Ordering by the transaction id keeps the sweep deterministic, so several
// workers stepping through the same backlog do not pile onto one row.
func (r *Repo) DuePending(ctx context.Context, limit int) ([]*Entity, error) {
	limit = repository.NormalizeLimit(limit)
	const query = `
		SELECT ` + columns + ` FROM ` + TableName + `
		 WHERE status = 'PENDING_REFERENCE'
		   AND next_attempt_at IS NOT NULL
		   AND next_attempt_at <= $1
		 ORDER BY id ASC
		 LIMIT $2`
	rows, err := r.resolver.Q(ctx).Query(ctx, query, time.Now().UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("wagertransactiondetails: due pending: %w", err)
	}
	defer rows.Close()

	var out []*Entity
	for rows.Next() {
		entity := &Entity{}
		if err := rows.Scan(scanTargets(entity)...); err != nil {
			return nil, fmt.Errorf("wagertransactiondetails: scan transaction: %w", err)
		}
		out = append(out, entity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("wagertransactiondetails: due pending: %w", err)
	}
	return out, nil
}

func (r *Repo) scanOne(ctx context.Context, where string, args ...any) (*Entity, error) {
	entity := &Entity{}
	err := r.resolver.Q(ctx).QueryRow(ctx, "SELECT "+columns+" FROM "+TableName+" "+where, args...).
		Scan(scanTargets(entity)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("wagertransactiondetails: transaction not found: %w", repository.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("wagertransactiondetails: read transaction: %w", err)
	}
	return entity, nil
}

// Update upserts a transaction row. It exists for projection rebuilds; the
// normal path is the projector, which writes explicit statements so every
// transition stays auditable.
func (r *Repo) Update(ctx context.Context, id string, model *Entity) error {
	const upsert = `
		INSERT INTO ` + TableName + ` (
			id, origin, provider_id, external_transaction_id, idempotency_key, payload_hash,
			wallet_id, player_id, round_id, game_id, kind, amount_minor, currency,
			reference_external_id, reference_transaction_id, status, failure_code, failure_message,
			balance_after_minor, balance_after_currency, pending_attempts, next_attempt_at,
			created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			failure_code = EXCLUDED.failure_code,
			failure_message = EXCLUDED.failure_message,
			reference_transaction_id = EXCLUDED.reference_transaction_id,
			balance_after_minor = EXCLUDED.balance_after_minor,
			balance_after_currency = EXCLUDED.balance_after_currency,
			pending_attempts = EXCLUDED.pending_attempts,
			next_attempt_at = EXCLUDED.next_attempt_at,
			updated_at = EXCLUDED.updated_at`
	_, err := r.resolver.Q(ctx).Exec(ctx, upsert,
		model.ID, model.Origin, nullString(model.ProviderID), nullString(model.ExternalID),
		nullString(model.IdempotencyKey), model.PayloadHash, model.WalletID, model.PlayerID,
		nullString(model.RoundID), nullString(model.GameID), model.Kind, model.AmountMinor,
		model.Currency, nullString(model.ReferenceExternal), model.ReferenceInternal,
		model.Status, nullString(model.FailureCode), nullString(model.FailureMessage),
		model.BalanceAfterMinor, nullString(model.BalanceAfterCur), model.PendingAttempts,
		model.NextAttemptAt, model.CreatedAt, model.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("wagertransactiondetails: update transaction: %w", err)
	}
	return nil
}

// scanTargets lists the destinations of one row.
//
// Nullable columns are scanned through coalesce/pointer helpers: a row that was
// never rejected has no failure code, and a transaction that has not moved money
// has no resulting balance. Reading NULL into a plain string would fail the whole
// query for a perfectly normal row.
func scanTargets(entity *Entity) []any {
	return []any{
		&entity.ID, &entity.Origin,
		(*text)(&entity.ProviderID), (*text)(&entity.ExternalID), (*text)(&entity.IdempotencyKey),
		&entity.PayloadHash, &entity.WalletID, &entity.PlayerID,
		(*text)(&entity.RoundID), (*text)(&entity.GameID),
		&entity.Kind, &entity.AmountMinor, &entity.Currency, (*text)(&entity.ReferenceExternal),
		&entity.ReferenceInternal, &entity.Status,
		(*text)(&entity.FailureCode), (*text)(&entity.FailureMessage),
		&entity.BalanceAfterMinor, (*text)(&entity.BalanceAfterCur), &entity.PendingAttempts,
		&entity.NextAttemptAt, &entity.CreatedAt, &entity.UpdatedAt,
	}
}

// text is a nullable text column read as a plain string.
type text string

// Scan implements sql.Scanner.
func (t *text) Scan(src any) error {
	switch value := src.(type) {
	case nil:
		*t = ""
	case string:
		*t = text(value)
	case []byte:
		*t = text(value)
	default:
		return fmt.Errorf("wagertransactiondetails: cannot scan %T into text", src)
	}
	return nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
