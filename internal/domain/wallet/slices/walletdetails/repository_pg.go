package walletdetails

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5"

	"github.com/ironledger/iron-ledger/internal/platform/pgdb"
	"github.com/ironledger/iron-ledger/internal/platform/repository"
)

// Repo is the PostgreSQL implementation of WalletDetailsRepository.
type Repo struct{ resolver pgdb.Resolver }

// NewRepo builds the repository.
func NewRepo(resolver pgdb.Resolver) *Repo { return &Repo{resolver: resolver} }

var _ WalletDetailsRepository = (*Repo)(nil)

const columns = `id, player_id, currency, balance_minor, version, created_at, updated_at`

// Find returns the wallet with the given id.
func (r *Repo) Find(ctx context.Context, id string) (*WalletDetailsEntity, error) {
	walletID, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("walletdetails: %q is not a wallet id: %w", id, repository.ErrNotFound)
	}
	return r.find(ctx, "WHERE id = $1", walletID)
}

// FindByPlayer returns the wallet of a player in a currency.
func (r *Repo) FindByPlayer(ctx context.Context, playerID uuid.UUID, currency string) (*WalletDetailsEntity, error) {
	return r.find(ctx, "WHERE player_id = $1 AND currency = $2", playerID, currency)
}

func (r *Repo) find(ctx context.Context, where string, args ...any) (*WalletDetailsEntity, error) {
	entity := &WalletDetailsEntity{}
	err := r.resolver.Q(ctx).QueryRow(ctx,
		"SELECT "+columns+" FROM "+TableName+" "+where, args...,
	).Scan(&entity.ID, &entity.PlayerID, &entity.Currency, &entity.BalanceMinor,
		&entity.Version, &entity.CreatedAt, &entity.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("walletdetails: wallet not found: %w", repository.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("walletdetails: read wallet: %w", err)
	}
	return entity, nil
}

// Update upserts a wallet row. It is the convergence path used when a
// projection is rebuilt.
func (r *Repo) Update(ctx context.Context, id string, model *WalletDetailsEntity) error {
	const upsert = `
		INSERT INTO ` + TableName + ` (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE
		   SET balance_minor = EXCLUDED.balance_minor,
		       version      = EXCLUDED.version,
		       updated_at   = EXCLUDED.updated_at`
	_, err := r.resolver.Q(ctx).Exec(ctx, upsert,
		model.ID, model.PlayerID, model.Currency, model.BalanceMinor, model.Version,
		model.CreatedAt, model.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("walletdetails: update wallet: %w", err)
	}
	return nil
}
