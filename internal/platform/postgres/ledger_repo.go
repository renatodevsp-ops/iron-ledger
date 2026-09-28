package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// LedgerRepo implements domain.LedgerRepository over one transaction handle (or
// the pool, for read-only use). Every method is explicit SQL: there is no ORM
// and no query builder, so the statements, their locking clauses and their
// constraints are all visible at the call site.
type LedgerRepo struct {
	q querier
}

// NewLedgerRepo binds a ledger repository to a transaction handle.
func NewLedgerRepo(q querier) *LedgerRepo { return &LedgerRepo{q: q} }

// querier is the subset of pgx that both *pgxpool.Pool and pgx.Tx satisfy, so a
// repository can run inside a transaction or stand alone.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Timestamps cross the repository boundary as epoch nanoseconds (float8), so
// the domain never carries a time.Time and no timezone ambiguity enters the
// ledger.
const walletColumns = `id, tenant_id, player_id, currency, balance_minor, status, version, ` +
	`(EXTRACT(EPOCH FROM updated_at)::float8 * 1000000000)`

// LockWallet takes the per-wallet row lock. It is the first statement of every
// mutating transaction: the lock scope IS the wallet, which is what makes
// distinct wallets run concurrently (Constitution Principle VII).
func (r *LedgerRepo) LockWallet(ctx context.Context, walletID string) (*domain.Wallet, error) {
	return r.scanWallet(r.q.QueryRow(ctx,
		`SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, walletID))
}

// FindWallet reads a wallet without taking a lock. Balance reads use this.
func (r *LedgerRepo) FindWallet(ctx context.Context, walletID string) (*domain.Wallet, error) {
	return r.scanWallet(r.q.QueryRow(ctx,
		`SELECT `+walletColumns+` FROM wallets WHERE id = $1`, walletID))
}

func (r *LedgerRepo) scanWallet(row pgx.Row) (*domain.Wallet, error) {
	var (
		w            domain.Wallet
		currency     string
		status       string
		balanceMinor int64
		updatedAt    float64
	)
	err := row.Scan(&w.ID, &w.TenantID, &w.PlayerID, &currency, &balanceMinor, &status, &w.Version, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrWalletNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan wallet: %w", err)
	}
	w.Currency = domain.Currency(currency)
	w.Balance = domain.Money{AmountMinor: balanceMinor, Currency: w.Currency}
	w.Status = domain.WalletStatus(status)
	w.UpdatedAt = int64(updatedAt)
	return &w, nil
}

// UpdateBalance applies a signed delta atomically. The CHECK on
// wallets.balance_minor is the database's last line of defence, so a bug in the
// caller still cannot produce a negative balance.
func (r *LedgerRepo) UpdateBalance(ctx context.Context, walletID string, delta int64) error {
	tag, err := r.q.Exec(ctx,
		`UPDATE wallets SET balance_minor = balance_minor + $2, version = version + 1 WHERE id = $1`,
		walletID, delta)
	if err != nil {
		// A check violation here is the database catching a negative balance the
		// domain should already have refused, so it is reported as insufficient
		// funds rather than an internal fault.
		return TranslateWriteError(fmt.Errorf("update balance: %w", err))
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrWalletNotFound
	}
	return nil
}

// NextSequence returns the next per-wallet ledger sequence. The caller holds
// the wallet row lock, so this read cannot race another writer for the wallet.
func (r *LedgerRepo) NextSequence(ctx context.Context, walletID string) (int64, error) {
	var seq int64
	err := r.q.QueryRow(ctx,
		`SELECT COALESCE(MAX(sequence), 0) + 1 FROM ledger_entries WHERE wallet_id = $1`,
		walletID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("next sequence: %w", err)
	}
	return seq, nil
}

// AppendEntry writes one immutable ledger line. There is no update or delete
// path anywhere in this repository: a correction is a new entry referencing the
// original (Constitution Principle II).
func (r *LedgerRepo) AppendEntry(ctx context.Context, entry domain.LedgerEntry) error {
	tag, err := r.q.Exec(ctx, `
		INSERT INTO ledger_entries (
			entry_uid, wallet_id, tenant_id, operation_id, bet_id, entry_type, direction,
			amount_minor, currency, balance_after_minor, reverses_entry_id, sequence, occurred_at
		) VALUES (
			$1, $2, $3, $4, NULLIF($5, '')::UUID, $6, $7,
			$8, $9, $10, NULLIF($11, '')::BIGINT, $12, to_timestamp($13::float8 / 1e9)
		)`,
		entry.EntryID, entry.WalletID, entry.TenantID, entry.OperationID, entry.BetID,
		string(entry.EntryType), int16(entry.Direction), entry.AmountMinor, string(entry.Currency),
		entry.BalanceAfterMinor, entry.ReversesEntryID, entry.Sequence, entry.OccurredAtUnixNano)
	if err != nil {
		return TranslateWriteError(fmt.Errorf("append ledger entry: %w", err))
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("append ledger entry: expected 1 row, got %d", tag.RowsAffected())
	}
	return nil
}

const betColumns = `id, wallet_id, tenant_id, external_bet_ref, stake_minor, currency, status,
	COALESCE(settled_by_operation_id::TEXT, ''),
	(EXTRACT(EPOCH FROM created_at)::float8 * 1000000000),
	COALESCE(EXTRACT(EPOCH FROM settled_at)::float8 * 1000000000, 0)`

// CreateBet registers a new bet.
func (r *LedgerRepo) CreateBet(ctx context.Context, bet domain.Bet) error {
	_, err := r.q.Exec(ctx, `
		INSERT INTO bets (id, wallet_id, tenant_id, external_bet_ref, stake_minor, currency, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		bet.ID, bet.WalletID, bet.TenantID, bet.ExternalBetRef, bet.Stake.AmountMinor,
		string(bet.Stake.Currency), string(bet.Status))
	if err != nil {
		return translateBetError(err)
	}
	return nil
}

// GetBet reads a bet without taking a lock.
func (r *LedgerRepo) GetBet(ctx context.Context, betID string) (*domain.Bet, error) {
	return r.scanBet(r.q.QueryRow(ctx, `SELECT `+betColumns+` FROM bets WHERE id = $1`, betID))
}

// LockBet reads a bet with FOR UPDATE, so two concurrent settlements serialize
// on the bet row and the second one sees the first one's status.
func (r *LedgerRepo) LockBet(ctx context.Context, betID string) (*domain.Bet, error) {
	return r.scanBet(r.q.QueryRow(ctx, `SELECT `+betColumns+` FROM bets WHERE id = $1 FOR UPDATE`, betID))
}

func (r *LedgerRepo) scanBet(row pgx.Row) (*domain.Bet, error) {
	var (
		b          domain.Bet
		currency   string
		status     string
		stakeMinor int64
		createdAt  float64
		settledAt  float64
	)
	err := row.Scan(&b.ID, &b.WalletID, &b.TenantID, &b.ExternalBetRef, &stakeMinor, &currency,
		&status, &b.SettledByOperationID, &createdAt, &settledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrBetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan bet: %w", err)
	}
	b.Stake = domain.Money{AmountMinor: stakeMinor, Currency: domain.Currency(currency)}
	b.Status = domain.BetStatus(status)
	b.CreatedAt = int64(createdAt)
	b.SettledAt = int64(settledAt)
	return &b, nil
}

// SetBetStatus persists a status transition. The partial unique index
// bets_one_settlement and operations_one_settlement_per_bet both reject a
// second settlement, so a race loses at the database rather than in a check
// written by hand (FR-020).
func (r *LedgerRepo) SetBetStatus(ctx context.Context, bet domain.Bet) error {
	tag, err := r.q.Exec(ctx, `
		UPDATE bets
		SET status = $2,
			settled_by_operation_id = NULLIF($3, '')::UUID,
			settled_at = CASE WHEN $4::int8 = 0 THEN NULL ELSE to_timestamp($4::float8 / 1e9) END
		WHERE id = $1`,
		bet.ID, string(bet.Status), bet.SettledByOperationID, bet.SettledAt)
	if err != nil {
		return translateBetError(err)
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrBetNotFound
	}
	return nil
}

// FindBetByExternalRef resolves a caller's external bet reference within a
// tenant.
func (r *LedgerRepo) FindBetByExternalRef(ctx context.Context, tenantID, ref string) (*domain.Bet, error) {
	return r.scanBet(r.q.QueryRow(ctx,
		`SELECT `+betColumns+` FROM bets WHERE tenant_id = $1 AND external_bet_ref = $2`, tenantID, ref))
}

// FindEntriesByOperation lists the entries a previous operation appended, in
// sequence order. A ROLLBACK uses it to compute the compensating entries.
func (r *LedgerRepo) FindEntriesByOperation(ctx context.Context, operationID string) ([]domain.LedgerEntry, error) {
	rows, err := r.q.Query(ctx, `
		SELECT entry_uid::TEXT, wallet_id::TEXT, tenant_id, operation_id::TEXT,
			COALESCE(bet_id::TEXT, ''), entry_type, direction, amount_minor, currency,
			balance_after_minor, COALESCE(reverses_entry_id::TEXT, ''), sequence,
			(EXTRACT(EPOCH FROM occurred_at)::float8 * 1000000000)
		FROM ledger_entries
		WHERE operation_id = $1
		ORDER BY sequence ASC`, operationID)
	if err != nil {
		return nil, fmt.Errorf("find entries by operation: %w", err)
	}
	return collectEntries(rows)
}

// CountEntriesByType counts entries of a type for a bet, which is how a second
// refund of the same debit is refused without a hand-written flag.
func (r *LedgerRepo) CountEntriesByType(ctx context.Context, walletID, betID string, t domain.EntryType) (int64, error) {
	var n int64
	err := r.q.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries
		WHERE wallet_id = $1 AND bet_id = NULLIF($2, '')::UUID AND entry_type = $3`,
		walletID, betID, string(t)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count entries by type: %w", err)
	}
	return n, nil
}

func collectEntries(rows pgx.Rows) ([]domain.LedgerEntry, error) {
	defer rows.Close()
	var out []domain.LedgerEntry
	for rows.Next() {
		var (
			e          domain.LedgerEntry
			currency   string
			entryType  string
			direction  int16
			occurredAt float64
		)
		if err := rows.Scan(&e.EntryID, &e.WalletID, &e.TenantID, &e.OperationID, &e.BetID,
			&entryType, &direction, &e.AmountMinor, &currency, &e.BalanceAfterMinor,
			&e.ReversesEntryID, &e.Sequence, &occurredAt); err != nil {
			return nil, fmt.Errorf("scan ledger entry: %w", err)
		}
		e.EntryType = domain.EntryType(entryType)
		e.Direction = domain.Direction(direction)
		e.Currency = domain.Currency(currency)
		e.OccurredAtUnixNano = int64(occurredAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// translateBetError maps the unique violations the bets and operations tables
// raise onto the stable domain reason codes, so a constraint proves the rule
// instead of surfacing as an opaque SQL error.
func translateBetError(err error) error {
	switch PgErrorCode(err) {
	case "23505":
		return domain.NewError(domain.ReasonReferenceAlreadyExists, "reference already exists")
	case "23514":
		return domain.NewError(domain.ReasonInvalidAmount, "a database constraint rejected the bet state")
	default:
		return err
	}
}
