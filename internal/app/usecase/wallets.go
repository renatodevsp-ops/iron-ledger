package usecase

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/app/pipeline"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/registerwageroperation"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/settlewageroperation"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/openwallet"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/walletdetails"
	"github.com/ironledger/iron-ledger/internal/platform/config"
	"github.com/ironledger/iron-ledger/internal/platform/logging"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
	"github.com/ironledger/iron-ledger/internal/platform/repository"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/idempotency"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// WalletView is the wire form of a wallet.
type WalletView struct {
	ID        string     `json:"id"`
	PlayerID  string     `json:"playerId"`
	Balance   *MoneyView `json:"balance"`
	Version   int64      `json:"version"`
	CreatedAt string     `json:"createdAt"`
	UpdatedAt string     `json:"updatedAt"`
}

// OpenWalletRequest is the intent to open a player's wallet.
type OpenWalletRequest struct {
	PlayerID      uuid.UUID
	Initial       money.Money
	CorrelationID string
}

// WalletsUseCase opens wallets and reconciles them.
//
// Opening is an internal operation: only the platform's own service may call
// it. A provider can move money in an existing wallet but cannot bring one
// into existence.
type WalletsUseCase struct {
	tx      *uow.Manager
	pipe    *pipeline.Pipeline
	cfg     config.Wagering
	logger  *slog.Logger
	metrics *metrics.Metrics

	openWallet cqrs.CommandHandler[openwallet.Command]
	register   cqrs.CommandHandler[registerwageroperation.Command]
	settle     cqrs.CommandHandler[settlewageroperation.Command]

	wallets WalletReader
}

// NewWalletsUseCase wires the wallet use case to the slices it composes.
func NewWalletsUseCase(
	tx *uow.Manager,
	pipe *pipeline.Pipeline,
	cfg config.Wagering,
	logger *slog.Logger,
	m *metrics.Metrics,
	openWallet cqrs.CommandHandler[openwallet.Command],
	register cqrs.CommandHandler[registerwageroperation.Command],
	settle cqrs.CommandHandler[settlewageroperation.Command],
	wallets WalletReader,
) *WalletsUseCase {
	return &WalletsUseCase{
		tx:         tx,
		pipe:       pipe,
		cfg:        cfg,
		logger:     logger,
		metrics:    m,
		openWallet: openWallet,
		register:   register,
		settle:     settle,
		wallets:    wallets,
	}
}

// Open creates a wallet for a player in a single currency.
//
// A positive opening balance produces, in one commit: the wallet at version 1,
// one OPENING wager transaction in PROCESSED, one credit ledger entry, and the
// outbound WagerTransactionProcessed and WalletBalanceChanged events. A zero
// opening balance produces only the wallet: there is no value to record, so
// there is no opening transaction, no ledger entry and no financial event.
func (uc *WalletsUseCase) Open(ctx context.Context, req OpenWalletRequest) (*WalletView, error) {
	if req.PlayerID == uuid.Nil {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Missing player",
			"$field is required and must be a UUID.", map[string]any{"field": "playerId"})
	}
	if !req.Initial.Valid() {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Invalid initial balance",
			"$field must carry a known ISO 4217 currency.", map[string]any{"field": "initialBalance.currency"})
	}
	if req.Initial.IsNegative() {
		return nil, xerr.Validation(xerr.CodeNegativeAmountNotAllowed, "Negative initial balance",
			"$initial must be zero or positive. Open the wallet and credit it with an operation instead.",
			map[string]any{"initial": req.Initial.String()})
	}

	walletID := uuid.New()
	openingTx := uuid.New()
	ctx = logging.WithWalletID(ctx, walletID.String())
	ctx = logging.WithTransactionID(ctx, openingTx.String())
	ctx = cqrs.WithCausation(ctx, openingTx.String())

	err := uc.tx.Do(ctx, uow.Options{Name: "open_wallet"}, func(ctx context.Context) error {
		// The unique index on (player_id, currency) is the real guard against a
		// duplicated initial credit; checking first turns the race into a clear
		// 409 instead of a constraint error.
		existing, err := uc.wallets.FindByPlayer(ctx, req.PlayerID, string(req.Initial.Currency()))
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return unavailable(err)
		}
		if existing != nil {
			return xerr.Conflict(xerr.CodeWalletAlreadyExists, "Wallet already exists",
				"Player $player already has a wallet in $currency.",
				map[string]any{
					"player":   req.PlayerID.String(),
					"currency": string(req.Initial.Currency()),
					"wallet":   existing.ID.String(),
				})
		}

		if _, err := uc.openWallet(ctx, openwallet.Command{
			WalletID:      walletID,
			PlayerID:      req.PlayerID,
			OpeningTx:     openingTx,
			Initial:       req.Initial,
			CorrelationID: logging.CorrelationID(ctx),
		}); err != nil {
			return err
		}
		if err := uc.pipe.Flush(ctx); err != nil {
			return err
		}

		if req.Initial.IsPositive() {
			if err := uc.recordOpening(ctx, openingTx, walletID, req); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		uc.count("open_wallet", "error")
		return nil, err
	}

	wallet, err := uc.wallets.Find(ctx, walletID.String())
	if err != nil {
		return nil, unavailable(err)
	}
	uc.count("open_wallet", "created")
	return viewOf(wallet), nil
}

// recordOpening writes the internal OPENING transaction and settles it.
func (uc *WalletsUseCase) recordOpening(ctx context.Context, openingTx, walletID uuid.UUID, req OpenWalletRequest) error {
	if _, err := uc.register(ctx, registerwageroperation.Command{
		TransactionID: openingTx,
		Origin:        domain.OriginInternal,
		WalletID:      walletID,
		PlayerID:      req.PlayerID,
		Kind:          domain.KindOpening,
		Money:         req.Initial,
		PayloadHash:   openingHash(req.PlayerID, req.Initial),
		CorrelationID: logging.CorrelationID(ctx),
	}); err != nil {
		return err
	}
	if err := uc.pipe.Flush(ctx); err != nil {
		return err
	}
	if _, err := uc.settle(ctx, settlewageroperation.Command{
		TransactionID: openingTx,
		Outcome: settlewageroperation.Outcome{
			Processed:    true,
			BalanceAfter: req.Initial,
		},
		CorrelationID: logging.CorrelationID(ctx),
	}); err != nil {
		return err
	}
	return uc.pipe.Flush(ctx)
}

// Get returns a wallet.
func (uc *WalletsUseCase) Get(ctx context.Context, walletID uuid.UUID) (*WalletView, error) {
	wallet, err := uc.wallets.Find(ctx, walletID.String())
	if err != nil {
		return nil, unavailable(err)
	}
	return viewOf(wallet), nil
}

func viewOf(wallet *walletdetails.WalletDetailsEntity) *WalletView {
	balance := wallet.Balance()
	return &WalletView{
		ID:        wallet.ID.String(),
		PlayerID:  wallet.PlayerID.String(),
		Balance:   moneyView(&balance),
		Version:   wallet.Version,
		CreatedAt: wallet.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: wallet.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// openingHash fingerprints the internal opening. It uses the same algorithm as
// an external operation so the two are comparable in an audit, with the fields
// that do not apply to an internal operation left empty.
func openingHash(playerID uuid.UUID, initial money.Money) string {
	payload := idempotency.Payload{
		PlayerID: playerID.String(),
		Kind:     string(domain.KindOpening),
		Amount:   initial.String(),
		Currency: string(initial.Currency()),
	}
	hash, err := idempotency.Hash(payload)
	if err != nil {
		// The payload is a fixed shape of strings; it cannot fail to encode.
		return ""
	}
	return hash
}

func (uc *WalletsUseCase) count(operation, outcome string) {
	if uc.metrics != nil {
		uc.metrics.OperationResults.WithLabelValues(operation, outcome).Inc()
	}
}
