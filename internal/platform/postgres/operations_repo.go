package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// OperationsRepo implements domain.OperationsRepository. The idempotency claim
// is the mechanism the whole exactly-once story rests on, so it is a single
// explicit statement whose rows-affected count is the branch
// (research.md D-6).
type OperationsRepo struct {
	q querier
}

func NewOperationsRepo(q querier) *OperationsRepo { return &OperationsRepo{q: q} }

// operationColumns reads result_body as text rather than as jsonb. A jsonb value
// is stored in a normalized form with its keys reordered, so returning it raw
// would let the first response and a replay differ only in key order. Reading
// the same normalized text every time is what makes the replay byte-identical.
const operationColumns = `id, wallet_id::TEXT, tenant_id, idempotency_key, request_hash,
	operation_type, transaction_id, COALESCE(bet_id::TEXT, ''), amount, currency, status,
	result_code, result_body::TEXT, channel, actor,
	(EXTRACT(EPOCH FROM created_at)::float8 * 1000000000)`

// ClaimIdempotency inserts the idempotency record with ON CONFLICT DO NOTHING
// and reports whether this request is the first for the key. The insert happens
// in the same transaction as the financial effect, so a key is never marked
// processed without its effect and an effect is never applied without its key
// (Constitution Principle VI).
func (r *OperationsRepo) ClaimIdempotency(ctx context.Context, rec domain.OperationRecord) (bool, error) {
	tag, err := r.q.Exec(ctx, `
		INSERT INTO operations (
			id, wallet_id, tenant_id, idempotency_key, request_hash, operation_type,
			transaction_id, bet_id, amount, currency, status, result_code, result_body,
			channel, actor, created_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, NULLIF($8, '')::UUID, $9, $10, $11, $12, $13::JSONB,
			$14, $15, to_timestamp($16::float8 / 1e9)
		)
		ON CONFLICT (wallet_id, idempotency_key) DO NOTHING`,
		rec.ID, rec.WalletID, rec.TenantID, rec.IdempotencyKey, rec.RequestHash,
		string(rec.OperationType), rec.TransactionID, rec.BetID, rec.AmountMinor,
		string(rec.Currency), string(rec.Status), rec.ResultCode, string(rec.ResultBody),
		string(rec.Channel), rec.Actor, rec.CreatedUnixNano)
	if err != nil {
		if domErr := translateForeignKeyViolation(err); domErr != nil {
			return false, domErr
		}
		return false, fmt.Errorf("claim idempotency: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetByIdempotencyKey reads the stored original result for a replay. It returns
// (nil, nil) when the key is unknown, which is a normal outcome, not a failure.
func (r *OperationsRepo) GetByIdempotencyKey(ctx context.Context, walletID, key string) (*domain.OperationRecord, error) {
	return r.scanOperation(r.q.QueryRow(ctx,
		`SELECT `+operationColumns+` FROM operations WHERE wallet_id = $1 AND idempotency_key = $2`,
		walletID, key))
}

// FindByTransactionID reads the record for a caller transaction reference. It is
// how UNIQUE (wallet_id, transaction_id) is reported as a stable reason code.
// Returns (nil, nil) when the reference is unused.
func (r *OperationsRepo) FindByTransactionID(ctx context.Context, walletID, transactionID string) (*domain.OperationRecord, error) {
	return r.scanOperation(r.q.QueryRow(ctx,
		`SELECT `+operationColumns+` FROM operations WHERE wallet_id = $1 AND transaction_id = $2`,
		walletID, transactionID))
}

// FindByID loads the target of a ROLLBACK. Returns (nil, nil) when absent.
func (r *OperationsRepo) FindByID(ctx context.Context, operationID string) (*domain.OperationRecord, error) {
	return r.scanOperation(r.q.QueryRow(ctx,
		`SELECT `+operationColumns+` FROM operations WHERE id = $1`, operationID))
}

func (r *OperationsRepo) scanOperation(row pgx.Row) (*domain.OperationRecord, error) {
	var (
		rec      domain.OperationRecord
		currency string
		typ      string
		status   string
		channel  string
		body     string
		created  float64
	)
	err := row.Scan(&rec.ID, &rec.WalletID, &rec.TenantID, &rec.IdempotencyKey, &rec.RequestHash,
		&typ, &rec.TransactionID, &rec.BetID, &rec.AmountMinor, &currency, &status,
		&rec.ResultCode, &body, &channel, &rec.Actor, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan operation: %w", err)
	}
	rec.OperationType = domain.OperationType(typ)
	rec.Currency = domain.Currency(currency)
	rec.Status = domain.OperationStatus(status)
	rec.Channel = domain.Channel(channel)
	rec.ResultBody = []byte(body)
	rec.CreatedUnixNano = int64(created)
	return &rec, nil
}

// translateForeignKeyViolation turns a missing referenced row into the reason
// code the caller can act on, distinguishing a wallet the tenant cannot reach
// from a bet that does not exist.
func translateForeignKeyViolation(err error) *domain.Error {
	if PgErrorCode(err) != "23503" {
		return nil
	}
	switch PgErrorConstraint(err) {
	case "operations_bet_id_fkey":
		return domain.ErrBetNotFound
	case "operations_wallet_id_fkey":
		return domain.ErrWalletNotFound
	default:
		return domain.ErrWalletNotFound
	}
}

// PgErrorCode returns the SQLSTATE of a PostgreSQL error, or "" for other errors.
func PgErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// PgErrorConstraint returns the violated constraint name, or "" for other errors.
func PgErrorConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// PgErrorMessage returns the human-readable message of a PostgreSQL error.
func PgErrorMessage(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Message
	}
	return err.Error()
}

// TranslateWriteError maps a constraint violation onto the stable reason code
// the contract publishes, so a database guard surfaces as a business rejection
// rather than a 500. The constraint name decides the code: 23505 alone is
// ambiguous, and reporting "duplicate transaction" for a duplicate bet
// reference would be a lie.
func TranslateWriteError(err error) error {
	if err == nil {
		return nil
	}
	var domErr *domain.Error
	if errors.As(err, &domErr) {
		return err
	}
	switch PgErrorCode(err) {
	case "23505":
		switch PgErrorConstraint(err) {
		case "operations_wallet_txn_uniq":
			return domain.ErrDuplicateTransactionID
		case "bets_tenant_ref_uniq":
			return domain.ErrReferenceAlreadyExists
		case "bets_one_settlement", "operations_one_settlement_per_bet":
			return domain.ErrBetAlreadySettled
		case "ledger_entries_uid_uniq":
			// A generated UUID collided. That is a broken identifier source, not
			// something the caller can fix by changing the request.
			return domain.NewError(domain.ReasonInternalError,
				"generated ledger entry id collided: %s", PgErrorMessage(err))
		default:
			return domain.NewError(domain.ReasonInvalidOperationState,
				"operation violates unique constraint %s", PgErrorConstraint(err))
		}
	case "23503":
		if domErr := translateForeignKeyViolation(err); domErr != nil {
			return domErr
		}
	case "23514":
		// A check violation means the non-negative balance guard fired, which is
		// the database's version of insufficient funds.
		return domain.NewError(domain.ReasonInsufficientFunds,
			"a database constraint rejected the operation: %s", PgErrorMessage(err))
	case "23001":
		return domain.NewError(domain.ReasonInvalidOperationState,
			"the record is append-only and cannot be modified")
	default:
		return err
	}
	return err
}
