// Package openwallet is the STATE_CHANGE slice that opens a player's wallet.
//
// Stream: the Wallet aggregate, created empty. decide validates the opening
// terms and emits WalletOpened plus, when the wallet is opened with a positive
// balance, the WalletBalanceChanged that credits it — both on the same stream,
// in the same commit as the wallet row, its ledger entry and the outbound
// events they produce.
package openwallet

import (
	"context"
	"time"

	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/domain/wallet/domain"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/events"
	"github.com/ironledger/iron-ledger/internal/platform/support"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

var _ cqrs.Command = (*Command)(nil)

// Command expresses the intent to open a wallet for a player.
//
// OpeningTx is the stable internal identity of the opening operation. It is
// what the credit ledger entry and the WagerTransactionProcessed event point
// at, which is how the platform prevents the same initial credit twice even
// though two callers may race to open the wallet.
type Command struct {
	WalletID      uuid.UUID
	PlayerID      uuid.UUID
	OpeningTx     uuid.UUID
	Initial       money.Money
	CorrelationID string
}

// AggregateID identifies the Wallet aggregate this command targets.
func (c Command) AggregateID() string { return c.WalletID.String() }

// CommandType is the routing name of the command.
func (c Command) CommandType() string { return "OpenWallet" }

// State is everything decide needs to know about the wallet stream. It is the
// aggregate's snapshot, folded event by event — never persisted directly.
type State struct {
	snapshot domain.Snapshot
	exists   bool
}

func initialState() State { return State{} }

// evolve folds one historical event into the current state. It is pure: no
// movement, no validation, no side effect.
func evolve(s State, envelope *cqrs.Envelope) State {
	switch event := envelope.Event.(type) {
	case *events.WalletOpened:
		s.exists = true
		s.snapshot = domain.Snapshot{
			ID:        event.WalletID,
			PlayerID:  event.PlayerID,
			Currency:  event.Balance.Currency(),
			Balance:   event.Balance,
			Version:   domain.InitialVersion,
			CreatedAt: envelope.OccurredAt,
			UpdatedAt: envelope.OccurredAt,
		}
	case *events.WalletBalanceChanged:
		s.snapshot.Balance = event.BalanceAfter
		s.snapshot.Version = event.WalletVersion
		s.snapshot.UpdatedAt = envelope.OccurredAt
	}
	return s
}

// decide validates the opening terms and returns the events to append.
func decide(s State, cmd Command) ([]cqrs.Event, error) {
	if s.exists {
		return nil, xerr.Conflict(xerr.CodeWalletAlreadyExists, "Wallet already exists",
			"Wallet $wallet is already open for this player and currency.",
			map[string]any{"wallet": cmd.WalletID.String()})
	}
	if cmd.OpeningTx == uuid.Nil {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Missing opening transaction",
			"$field is required so the initial credit can be tied to a ledger entry.",
			map[string]any{"field": "openingTransactionId"})
	}

	occurredAt := time.Now().UTC()
	wallet, err := domain.Open(cmd.WalletID, cmd.PlayerID, cmd.Initial.Currency(), cmd.Initial, occurredAt)
	if err != nil {
		return nil, err
	}

	appended := []cqrs.Event{
		&events.WalletOpened{
			WalletID:  wallet.ID(),
			PlayerID:  wallet.PlayerID(),
			OpeningTx: cmd.OpeningTx,
			Balance:   wallet.Balance(),
		},
	}

	// A wallet opened at zero records no value: there is no balance to change,
	// so no ledger entry and no financial event. The version stays at 1.
	if wallet.Balance().IsPositive() {
		appended = append(appended, &events.WalletBalanceChanged{
			WalletID:      wallet.ID(),
			TransactionID: cmd.OpeningTx,
			Direction:     domain.DirectionCredit,
			Money:         wallet.Balance(),
			BalanceBefore: money.Zero(wallet.Currency()),
			BalanceAfter:  wallet.Balance(),
			WalletVersion: wallet.Version(),
		})
	}

	return appended, nil
}

// NewCommandHandler builds the handler backing this slice.
func NewCommandHandler(store cqrs.EventStore, extractors ...func(context.Context) map[string]any) cqrs.CommandHandler[Command] {
	options := []cqrs.CommandHandlerOption{
		cqrs.WithStreamState(cqrs.NoStream{}),
		cqrs.WithStreamNamer(support.WalletStream),
	}
	options = append(options, withMetadata(extractors...)...)
	return cqrs.NewCommandHandler(store, initialState, evolve, decide, options...)
}

// withMetadata turns metadata extractors into command handler options. Every
// slice accepts them so the correlation and causation chain reaches the event
// store without any slice having to know where it came from.
func withMetadata(extractors ...func(context.Context) map[string]any) []cqrs.CommandHandlerOption {
	options := make([]cqrs.CommandHandlerOption, 0, len(extractors))
	for _, extract := range extractors {
		options = append(options, cqrs.WithMetadataExtractor(extract))
	}
	return options
}
