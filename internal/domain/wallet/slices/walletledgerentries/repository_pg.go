package walletledgerentries

import (
	"context"
	"errors"
	"fmt"

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

const columns = `id, wallet_id, transaction_id, direction, amount_minor,
	balance_before_minor, balance_after_minor, currency, created_at`

// Find returns a single entry by id.
func (r *Repo) Find(ctx context.Context, id string) (*EntryEntity, error) {
	entryID, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("walletledgerentries: %q is not an entry id: %w", id, repository.ErrNotFound)
	}
	entity := &EntryEntity{}
	err = r.resolver.Q(ctx).QueryRow(ctx, "SELECT "+columns+" FROM "+TableName+" WHERE id = $1", entryID).
		Scan(scanTargets(entity)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("walletledgerentries: entry not found: %w", repository.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("walletledgerentries: read entry: %w", err)
	}
	return entity, nil
}

// Page returns a page of entries, oldest first.
//
// The order is (created_at, id): the timestamp carries the sequence and the id
// breaks ties, so two entries written in the same millisecond still have a
// defined order and the cursor never skips or repeats one. An identifier alone
// would not do — no UUID is guaranteed to sort by creation time across
// processes.
func (r *Repo) Page(ctx context.Context, walletID uuid.UUID, after repository.PageCursor, limit int) (repository.Connection[EntryEntity], error) {
	limit = repository.NormalizeLimit(limit)

	rows, err := r.resolver.Q(ctx).Query(ctx,
		"SELECT "+columns+" FROM "+TableName+
			" WHERE wallet_id = $1 AND (created_at, id) > ($2, $3)"+
			" ORDER BY created_at ASC, id ASC LIMIT $4",
		walletID, after.CreatedAt, after.ID, limit,
	)
	if err != nil {
		return repository.Connection[EntryEntity]{}, fmt.Errorf("walletledgerentries: page: %w", err)
	}
	defer rows.Close()

	connection := repository.Connection[EntryEntity]{}
	for rows.Next() {
		entity := &EntryEntity{}
		if err := rows.Scan(scanTargets(entity)...); err != nil {
			return repository.Connection[EntryEntity]{}, fmt.Errorf("walletledgerentries: scan entry: %w", err)
		}
		connection.Nodes = append(connection.Nodes, entity)
	}
	if err := rows.Err(); err != nil {
		return repository.Connection[EntryEntity]{}, fmt.Errorf("walletledgerentries: page: %w", err)
	}

	// A short page means the caller has reached the end; a full page may still
	// have more, so hand back the cursor of the last row.
	if len(connection.Nodes) == limit {
		last := connection.Nodes[len(connection.Nodes)-1]
		connection.Cursor = repository.EncodePageCursor(repository.PageCursor{
			CreatedAt: last.CreatedAt,
			ID:        last.ID,
		})
	}
	return connection, nil
}

// SumByDirection aggregates the ledger of a wallet.
//
// It reads the ledger only: no wallet row is consulted, so it is an independent
// reconstruction of what the balance should be. The aggregate is done in SQL in
// one statement, and the caller reads the stored balance in the same
// transaction, so the two sides of a reconciliation see the same snapshot.
func (r *Repo) SumByDirection(ctx context.Context, walletID uuid.UUID) (credits, debits, count int64, err error) {
	const sum = `
		SELECT
			COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'CREDIT'), 0) AS credits,
			COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'DEBIT'), 0)  AS debits,
			COUNT(*)                                                             AS entries
		FROM ` + TableName + ` WHERE wallet_id = $1`
	err = r.resolver.Q(ctx).QueryRow(ctx, sum, walletID).Scan(&credits, &debits, &count)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("walletledgerentries: sum ledger: %w", err)
	}
	return credits, debits, count, nil
}

func scanTargets(entity *EntryEntity) []any {
	return []any{
		&entity.ID, &entity.WalletID, &entity.TransactionID, &entity.Direction,
		&entity.AmountMinor, &entity.BalanceBeforeMinor, &entity.BalanceAfterMinor,
		&entity.Currency, &entity.CreatedAt,
	}
}
