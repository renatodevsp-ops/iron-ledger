package usecase

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/walletledgerentries"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/walletreconciliation"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
	"github.com/ironledger/iron-ledger/internal/platform/repository"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// LedgerEntryView is the wire form of one ledger entry.
type LedgerEntryView struct {
	ID            string     `json:"id"`
	WalletID      string     `json:"walletId"`
	TransactionID string     `json:"transactionId"`
	Direction     string     `json:"direction"`
	Money         *MoneyView `json:"money"`
	BalanceBefore *MoneyView `json:"balanceBefore"`
	BalanceAfter  *MoneyView `json:"balanceAfter"`
	CreatedAt     string     `json:"createdAt"`
}

// LedgerPageView is one page of the ledger plus the cursor of the next page.
type LedgerPageView struct {
	Data       []*LedgerEntryView `json:"data"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

// LedgerUseCase reads the ledger and reconciles wallets against it.
type LedgerUseCase struct {
	tx        *uow.Manager
	ledger    walletledgerentries.Repository
	reconcile *walletreconciliation.Service
	logger    *slog.Logger
	metrics   *metrics.Metrics
}

// NewLedgerUseCase builds the ledger use case.
func NewLedgerUseCase(
	tx *uow.Manager,
	ledger walletledgerentries.Repository,
	reconcile *walletreconciliation.Service,
	logger *slog.Logger,
	m *metrics.Metrics,
) *LedgerUseCase {
	return &LedgerUseCase{tx: tx, ledger: ledger, reconcile: reconcile, logger: logger, metrics: m}
}

// List returns a page of ledger entries, oldest first.
func (uc *LedgerUseCase) List(ctx context.Context, walletID uuid.UUID, cursor string, limit int) (*LedgerPageView, error) {
	after, err := repository.DecodePageCursor(cursor)
	if err != nil {
		return nil, invalidCursor(cursor)
	}
	connection, err := uc.ledger.Page(ctx, walletID, after, limit)
	if err != nil {
		return nil, unavailable(err)
	}

	page := &LedgerPageView{NextCursor: connection.Cursor, Data: make([]*LedgerEntryView, 0, len(connection.Nodes))}
	for _, entry := range connection.Nodes {
		amount := entry.Amount()
		before := entry.BalanceBefore()
		afterBalance := entry.BalanceAfter()
		page.Data = append(page.Data, &LedgerEntryView{
			ID:            entry.ID.String(),
			WalletID:      entry.WalletID.String(),
			TransactionID: entry.TransactionID.String(),
			Direction:     entry.Direction,
			Money:         moneyView(&amount),
			BalanceBefore: moneyView(&before),
			BalanceAfter:  moneyView(&afterBalance),
			CreatedAt:     entry.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return page, nil
}

// Reconcile rebuilds a wallet's balance from its ledger and compares it with
// the stored balance.
//
// It runs inside a read transaction so both sides of the comparison describe
// the same instant, and it never writes: a reconciliation that could change the
// balance would destroy the evidence it exists to produce.
func (uc *LedgerUseCase) Reconcile(ctx context.Context, walletID uuid.UUID) (*walletreconciliation.Report, error) {
	var report *walletreconciliation.Report
	err := uc.tx.Do(ctx, uow.Options{Name: "reconcile_wallet", NoRetry: true}, func(ctx context.Context) error {
		result, err := uc.reconcile.Reconcile(ctx, walletID)
		if err != nil {
			return err
		}
		report = result
		return nil
	})
	if err != nil {
		return nil, unavailable(err)
	}

	if !report.Consistent && uc.logger != nil {
		uc.logger.Log(ctx, slog.LevelError, "wallet reconciliation diverged",
			"walletId", report.WalletID.String(),
			"stored", report.StoredBalance.String(),
			"calculated", report.CalculatedBalance.String(),
			"difference", report.Difference.String(),
			"entries", report.CheckedEntries,
		)
	}
	return report, nil
}

// ReconciliationObserver reports divergences to the metrics registry.
type ReconciliationObserver struct {
	Metrics *metrics.Metrics
}

// Divergence implements walletreconciliation.DivergenceObserver.
func (o ReconciliationObserver) Divergence(_ context.Context, report *walletreconciliation.Report) {
	if o.Metrics == nil {
		return
	}
	o.Metrics.ReconciliationDivergences.WithLabelValues(report.WalletID.String()).Inc()
}

func invalidCursor(cursor string) error {
	return xerr.Validation(xerr.CodeInvalidRequest, "Invalid cursor",
		"$cursor is not a cursor issued by this endpoint. Omit it to read the first page.",
		map[string]any{"cursor": cursor})
}
