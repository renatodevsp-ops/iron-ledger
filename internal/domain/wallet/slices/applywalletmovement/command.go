// Package applywalletmovement is the STATE_CHANGE slice that moves a wallet
// balance.
//
// Every financial movement in the platform — an opening credit, a bet debit, a
// win credit, a refund, the opposite movement of a rollback — funnels through
// this one command. Keeping a single writer for the balance is what makes the
// invariants hold: the aggregate decides whether the movement is legal, and
// the ledger entry, the balance and the outbound event are committed with the
// event that caused them.
package applywalletmovement

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

// Command expresses the intent to move a wallet balance by a fixed amount.
//
// Direction and Money are decided by the caller, but only the aggregate may
// apply them: decide refuses an overdraft, a currency mismatch or a movement
// that would leave the balance unchanged.
type Command struct {
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     domain.Direction
	Money         money.Money
	CorrelationID string
	CausationID   string
}

// AggregateID identifies the Wallet aggregate this command targets.
func (c Command) AggregateID() string { return c.WalletID.String() }

// CommandType is the routing name of the command.
func (c Command) CommandType() string { return "ApplyWalletMovement" }

// State is everything decide needs to know about the wallet stream.
type State struct {
	snapshot domain.Snapshot
	exists   bool
}

func initialState() State { return State{} }

// evolve folds one historical event into the current state.
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

// decide applies the movement to the aggregate and returns the event that
// records it.
func decide(s State, cmd Command) ([]cqrs.Event, error) {
	if !s.exists {
		return nil, xerr.NotFound(xerr.CodeWalletNotFound, "Wallet not found",
			"Wallet $wallet does not exist. Open it before applying operations.",
			map[string]any{"wallet": cmd.WalletID.String()})
	}

	wallet, err := domain.Rehydrate(s.snapshot)
	if err != nil {
		return nil, err
	}

	before := wallet.Balance()
	updated, err := wallet.Apply(domain.Movement{
		TransactionID: cmd.TransactionID,
		Direction:     cmd.Direction,
		Amount:        cmd.Money,
	}, time.Now().UTC())
	if err != nil {
		return nil, err
	}

	return []cqrs.Event{
		&events.WalletBalanceChanged{
			WalletID:      updated.ID(),
			TransactionID: cmd.TransactionID,
			Direction:     cmd.Direction,
			Money:         cmd.Money,
			BalanceBefore: before,
			BalanceAfter:  updated.Balance(),
			WalletVersion: updated.Version(),
		},
	}, nil
}

// NewCommandHandler builds the handler backing this slice.
func NewCommandHandler(store cqrs.EventStore, extractors ...func(context.Context) map[string]any) cqrs.CommandHandler[Command] {
	options := []cqrs.CommandHandlerOption{
		cqrs.WithStreamState(cqrs.Any{}),
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
