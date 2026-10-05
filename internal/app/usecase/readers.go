package usecase

import (
	"context"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/wagertransactiondetails"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/walletdetails"
)

// WalletReader is the slice of the wallet projection this use case needs.
//
// The use case depends on these narrow interfaces rather than on the
// repositories themselves: it states what it reads, so a slice can be replaced
// or replayed without touching the application layer.
type WalletReader interface {
	Find(ctx context.Context, id string) (*walletdetails.WalletDetailsEntity, error)
	FindByPlayer(ctx context.Context, playerID uuid.UUID, currency string) (*walletdetails.WalletDetailsEntity, error)
}

// TransactionReader is the slice of the wager transaction index this use case
// needs.
type TransactionReader interface {
	Find(ctx context.Context, id string) (*wagertransactiondetails.Entity, error)
	FindByIdempotencyKey(ctx context.Context, providerID, key string) (*wagertransactiondetails.Entity, error)
	FindByExternalID(ctx context.Context, providerID, externalID string) (*wagertransactiondetails.Entity, error)
	FindReversalOf(ctx context.Context, referenceID uuid.UUID) (*wagertransactiondetails.Entity, error)
	DuePending(ctx context.Context, limit int) ([]*wagertransactiondetails.Entity, error)
}

// LedgerReader is the slice of the ledger this package needs.
type LedgerReader interface {
	SumByDirection(ctx context.Context, walletID uuid.UUID) (credits, debits, count int64, err error)
}
