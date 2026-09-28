package usecase

import (
	"context"
	"time"

	"github.com/ironledger/ironledger/internal/domain"
)

// BalanceView is the read model behind GET /v1/wallets/{id}. The balance is
// materialized on the wallet row for speed, but it is always exactly equal to
// the sum of the wallet's ledger entries, so answering a caller never has to
// fold the ledger.
type BalanceView struct {
	WalletID     string
	PlayerID     string
	TenantID     string
	BalanceMinor int64
	Currency     domain.Currency
	Status       domain.WalletStatus
	Version      int64
	UpdatedAt    time.Time
}

// GetBalance reads a wallet balance without locking, so reads never queue behind
// writers.
func (s *Service) GetBalance(ctx context.Context, tenantID, walletID string) (BalanceView, error) {
	var view BalanceView
	err := s.uow.Do(ctx, func(ctx context.Context, tx domain.Tx) error {
		wallet, err := tx.Ledger.FindWallet(ctx, walletID)
		if err != nil {
			return err
		}
		if wallet.TenantID != tenantID {
			return domain.ErrTenantMismatch
		}
		view = BalanceView{
			WalletID:     wallet.ID,
			PlayerID:     wallet.PlayerID,
			TenantID:     wallet.TenantID,
			BalanceMinor: wallet.Balance.AmountMinor,
			Currency:     wallet.Currency,
			Status:       wallet.Status,
			Version:      wallet.Version,
			UpdatedAt:    s.clock.Now().UTC(),
		}
		return nil
	})
	if err != nil {
		return BalanceView{}, asDomainError(err)
	}
	return view, nil
}
