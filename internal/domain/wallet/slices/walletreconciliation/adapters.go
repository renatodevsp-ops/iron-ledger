package walletreconciliation

import (
	"context"

	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/walletdetails"
)

// WalletFinder is the projection read a reconciliation depends on.
type WalletFinder interface {
	Find(ctx context.Context, id string) (*walletdetails.WalletDetailsEntity, error)
}

// WalletAdapter narrows the wallet projection to the single fact a
// reconciliation needs, so this slice does not depend on the whole read model.
type WalletAdapter struct{ finder WalletFinder }

// NewWalletAdapter adapts the wallet projection for the reconciliation.
func NewWalletAdapter(finder WalletFinder) *WalletAdapter {
	return &WalletAdapter{finder: finder}
}

// Find returns the stored balance of a wallet.
func (a *WalletAdapter) Find(ctx context.Context, id string) (*WalletBalance, error) {
	entity, err := a.finder.Find(ctx, id)
	if err != nil {
		return nil, err
	}
	return &WalletBalance{
		ID:           entity.ID,
		Currency:     entity.Currency,
		BalanceMinor: entity.BalanceMinor,
	}, nil
}

var _ WalletBalances = (*WalletAdapter)(nil)
