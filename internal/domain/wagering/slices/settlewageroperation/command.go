// Package settlewageroperation is the STATE_CHANGE slice that closes a wager
// transaction.
//
// Stream: the WagerTransaction aggregate. One command covers both terminal
// outcomes — settled or rejected — because both are the same transition from the
// caller's point of view: this operation is over, here is why. Which one it is
// depends only on the outcome the wallet movement produced, never on how many
// times the platform tried.
package settlewageroperation

import (
	"context"
	"time"

	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/events"
	"github.com/ironledger/iron-ledger/internal/platform/support"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

var _ cqrs.Command = (*Command)(nil)

// Outcome is the decision the settlement carries.
type Outcome struct {
	// Processed marks the operation as successfully applied.
	Processed bool
	// BalanceAfter is the wallet balance observed after the movement. It is
	// only read when Processed is true.
	BalanceAfter money.Money
	// ResolvedReference is the internal transaction a reversal acted on, once
	// the external reference has been resolved.
	ResolvedReference *uuid.UUID
	// FailureCode and FailureMessage describe a rejection.
	FailureCode    xerr.Code
	FailureMessage string
}

// Command expresses the intent to close a wager transaction with a known
// outcome.
type Command struct {
	TransactionID uuid.UUID
	Outcome       Outcome
	CorrelationID string
	CausationID   string
}

// AggregateID identifies the WagerTransaction aggregate this command targets.
func (c Command) AggregateID() string { return c.TransactionID.String() }

// CommandType is the routing name of the command.
func (c Command) CommandType() string { return "SettleWagerOperation" }

// State is the transaction's position in its lifecycle, rebuilt from history.
type State struct {
	registered     bool
	status         domain.Status
	kind           domain.Kind
	money          money.Money
	walletID       uuid.UUID
	playerID       uuid.UUID
	roundID        string
	gameID         string
	providerID     string
	externalID     string
	referenceExt   *string
	referenceID    *uuid.UUID
	failureCode    xerr.Code
	failureMessage string
	createdAt      time.Time
	balanceAfter   *money.Money
}

func initialState() State { return State{} }

// evolve folds one historical event into the current state.
func evolve(s State, envelope *cqrs.Envelope) State {
	s.createdAt = envelope.OccurredAt.UTC()
	switch event := envelope.Event.(type) {
	case *events.WagerTransactionRegistered:
		s.registered = true
		s.status = domain.StatusPending
		s.kind = event.Kind
		s.money = event.Money
		s.walletID = event.WalletID
		s.playerID = event.PlayerID
		s.roundID = event.RoundID
		s.gameID = event.GameID
		s.providerID = event.ProviderID
		s.externalID = event.ExternalID
		s.referenceExt = event.ReferenceExtern
	case *events.WagerTransactionPendingReference:
		s.status = domain.StatusPendingReference
	case *events.WagerTransactionProcessed:
		s.status = domain.StatusProcessed
		s.referenceID = event.ResolvedReference
		balance := event.BalanceAfter
		s.balanceAfter = &balance
	case *events.WagerTransactionRejected:
		s.status = domain.StatusRejected
		s.failureCode = event.FailureCode
		s.failureMessage = event.FailureMessage
	case *events.WagerTransactionFailed:
		s.status = domain.StatusFailed
		s.failureCode = event.FailureCode
		s.failureMessage = event.FailureMessage
	}
	return s
}

// decide applies the outcome through the aggregate and returns the event that
// records it.
func decide(s State, cmd Command) ([]cqrs.Event, error) {
	if !s.registered {
		return nil, xerr.NotFound(xerr.CodeResourceNotFound, "Transaction not registered",
			"Transaction $transaction was never accepted, so it cannot be settled.",
			map[string]any{"transaction": cmd.TransactionID.String()})
	}

	tx, err := domain.Rehydrate(snapshotOf(s, cmd.TransactionID))
	if err != nil {
		return nil, err
	}

	if cmd.Outcome.Processed {
		settled, err := tx.Settle(cmd.Outcome.ResolvedReference, cmd.Outcome.BalanceAfter, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		return []cqrs.Event{
			&events.WagerTransactionProcessed{
				TransactionID:     cmd.TransactionID,
				ProviderID:        settled.ProviderID(),
				ExternalID:        settled.ExternalID(),
				WalletID:          settled.WalletID(),
				PlayerID:          settled.PlayerID(),
				RoundID:           settled.RoundID(),
				GameID:            settled.GameID(),
				Kind:              settled.Kind(),
				Money:             settled.Amount(),
				BalanceAfter:      cmd.Outcome.BalanceAfter,
				ResolvedReference: settled.ReferenceInternalID(),
				ReferenceExtern:   settled.ReferenceExternalID(),
			},
		}, nil
	}

	code := cmd.Outcome.FailureCode
	if code == "" {
		return nil, xerr.Infrastructure("settlement without an outcome", nil)
	}
	rejected, err := tx.Reject(code, cmd.Outcome.FailureMessage, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return []cqrs.Event{
		&events.WagerTransactionRejected{
			TransactionID:   cmd.TransactionID,
			ProviderID:      rejected.ProviderID(),
			ExternalID:      rejected.ExternalID(),
			WalletID:        rejected.WalletID(),
			PlayerID:        rejected.PlayerID(),
			RoundID:         rejected.RoundID(),
			GameID:          rejected.GameID(),
			Kind:            rejected.Kind(),
			Money:           rejected.Amount(),
			FailureCode:     code,
			FailureMessage:  cmd.Outcome.FailureMessage,
			ReferenceExtern: rejected.ReferenceExternalID(),
		},
	}, nil
}

func snapshotOf(s State, id uuid.UUID) domain.Snapshot {
	return domain.Snapshot{
		ID:                  id,
		Status:              s.status,
		Kind:                s.kind,
		Amount:              s.money,
		WalletID:            s.walletID,
		PlayerID:            s.playerID,
		RoundID:             s.roundID,
		GameID:              s.gameID,
		ProviderID:          s.providerID,
		ExternalID:          s.externalID,
		ReferenceExternalID: s.referenceExt,
		ReferenceInternalID: s.referenceID,
		FailureCode:         s.failureCode,
		FailureMessage:      s.failureMessage,
		BalanceAfter:        s.balanceAfter,
		CreatedAt:           s.createdAt,
		UpdatedAt:           s.createdAt,
	}
}

// NewCommandHandler builds the handler backing this slice.
func NewCommandHandler(store cqrs.EventStore, extractors ...func(context.Context) map[string]any) cqrs.CommandHandler[Command] {
	options := []cqrs.CommandHandlerOption{
		cqrs.WithStreamState(cqrs.Any{}),
		cqrs.WithStreamNamer(support.WagerTransactionStream),
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
