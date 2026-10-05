// Package walletreconciliation is the STATE_VIEW slice that proves a stored
// balance is the sum of its ledger.
//
// It reads two things — the balance column of the wallets row and the
// aggregate of the ledger — inside one transaction, and reports the
// difference. It never writes: a reconciliation that could change the balance
// would destroy the evidence it is looking for.
package walletreconciliation

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// Report is the outcome of a reconciliation.
type Report struct {
	WalletID          uuid.UUID   `json:"walletId"`
	Currency          string      `json:"currency"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int64       `json:"checkedEntries"`
}

// Query reconciles one wallet.
type Query struct {
	WalletID uuid.UUID
}

// ID implements cqrs.Query.
func (q Query) ID() []byte {
	return append([]byte("walletreconciliation:"), q.WalletID[:]...)
}

// Reconciler recomputes a balance from the ledger.
type Reconciler interface {
	Reconcile(ctx context.Context, walletID uuid.UUID) (*Report, error)
}

// Service is the read side of this slice.
type Service struct {
	ledgers  LedgerSums
	wallets  WalletBalances
	observer DivergenceObserver
}

// LedgerSums aggregates the ledger of a wallet.
type LedgerSums interface {
	SumByDirection(ctx context.Context, walletID uuid.UUID) (credits, debits, count int64, err error)
}

// WalletBalances reads the stored balance of a wallet.
type WalletBalances interface {
	Find(ctx context.Context, id string) (*WalletBalance, error)
}

// WalletBalance is the minimal wallet fact a reconciliation needs.
type WalletBalance struct {
	ID           uuid.UUID
	Currency     string
	BalanceMinor int64
}

// DivergenceObserver is notified when a reconciliation finds a mismatch.
type DivergenceObserver interface {
	Divergence(ctx context.Context, report *Report)
}

// New builds the reconciliation service.
func New(ledgers LedgerSums, wallets WalletBalances, observer DivergenceObserver) *Service {
	return &Service{ledgers: ledgers, wallets: wallets, observer: observer}
}

// Reconcile compares the stored balance with the balance the ledger implies.
//
// Both numbers are read in the same transaction, so they describe the same
// instant. Under READ COMMITTED a single statement already sees one snapshot;
// holding the transaction open across both reads keeps them consistent even if
// the wallet moves in between.
func (s *Service) Reconcile(ctx context.Context, walletID uuid.UUID) (*Report, error) {
	wallet, err := s.wallets.Find(ctx, walletID.String())
	if err != nil {
		if e, ok := xerr.As(err); ok {
			return nil, e
		}
		return nil, xerr.NotFound(xerr.CodeWalletNotFound, "Wallet not found",
			"Wallet $wallet does not exist.", map[string]any{"wallet": walletID.String()})
	}

	currency, err := money.ParseCurrency(wallet.Currency)
	if err != nil {
		return nil, xerr.Infrastructure("persisted wallet carries an invalid currency", err)
	}

	credits, debits, count, err := s.ledgers.SumByDirection(ctx, walletID)
	if err != nil {
		return nil, fmt.Errorf("walletreconciliation: %w", err)
	}

	calculated := money.FromMinor(credits-debits, currency)
	stored := money.FromMinor(wallet.BalanceMinor, currency)
	difference, err := stored.Sub(calculated)
	if err != nil {
		return nil, xerr.Infrastructure("reconciliation difference overflows", err)
	}

	report := &Report{
		WalletID:          walletID,
		Currency:          string(currency),
		StoredBalance:     stored,
		CalculatedBalance: calculated,
		Difference:        difference,
		Consistent:        difference.IsZero(),
		CheckedEntries:    count,
	}

	if !report.Consistent && s.observer != nil {
		s.observer.Divergence(ctx, report)
	}
	return report, nil
}

var (
	_ Reconciler                        = (*Service)(nil)
	_ cqrs.QueryHandler[Query, *Report] = (*Service)(nil)
)

// HandleQuery makes the service usable directly on the query bus.
func (s *Service) HandleQuery(ctx context.Context, qry Query) (*Report, error) {
	if qry.WalletID == uuid.Nil {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Missing wallet",
			"$field is required.", map[string]any{"field": "walletId"})
	}
	return s.Reconcile(ctx, qry.WalletID)
}
