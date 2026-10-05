// Package walletdetails is the STATE_VIEW slice behind the wallet read model.
//
// It projects the Wallet event stream into the wallets table — the row every
// read, every financial invariant check and every reconciliation compares
// against — and answers the wallet queries the API exposes.
package walletdetails

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/platform/repository"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
)

// TableName is the projection this slice owns.
const TableName = "wallets"

// WalletDetailsEntity is the projected wallet row.
//
// Balance is held in minor units; the API renders it as a decimal string. The
// entity never sees a float.
type WalletDetailsEntity struct {
	ID           uuid.UUID `json:"id"`
	PlayerID     uuid.UUID `json:"playerId"`
	Currency     string    `json:"currency"`
	BalanceMinor int64     `json:"balanceMinor"`
	Version      int64     `json:"version"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Balance returns the entity's balance as a Money value.
func (e *WalletDetailsEntity) Balance() money.Money {
	return money.FromMinor(e.BalanceMinor, money.Currency(e.Currency))
}

// WalletDetailsRepository is the projection a projector writes and a query
// handler reads.
type WalletDetailsRepository interface {
	repository.Repository[WalletDetailsEntity]
	// FindByPlayer resolves the single wallet a player holds in a currency.
	FindByPlayer(ctx context.Context, playerID uuid.UUID, currency string) (*WalletDetailsEntity, error)
}

// WalletDetailsQuery looks a wallet up by its own identity.
type WalletDetailsQuery struct {
	WalletID uuid.UUID
}

// ID implements cqrs.Query.
func (q WalletDetailsQuery) ID() []byte {
	return append([]byte("walletdetails:by-id:"), q.WalletID[:]...)
}

// WalletByPlayerQuery looks a wallet up by its owner and currency. It is how
// the platform refuses to open a second wallet for the same pair without
// waiting for the unique index to complain.
type WalletByPlayerQuery struct {
	PlayerID uuid.UUID
	Currency string
}

// ID implements cqrs.Query.
func (q WalletByPlayerQuery) ID() []byte {
	out := append([]byte("walletdetails:by-player:"), q.PlayerID[:]...)
	return append(out, []byte(":"+q.Currency)...)
}

// WalletDetailsReadModel is the result of a wallet lookup.
type WalletDetailsReadModel struct {
	Data *WalletDetailsEntity
}
