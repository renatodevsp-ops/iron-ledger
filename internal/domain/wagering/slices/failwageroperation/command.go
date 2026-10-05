// Package failwageroperation is the STATE_CHANGE slice that records a permanent
// infrastructure failure.
//
// A transaction must never be left in PENDING because the platform could not
// finish it: the attempt is recorded as FAILED with a stable code, so an
// operator can find it and so a replay of the provider's request reports a
// terminal result instead of retrying forever.
package failwageroperation

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

// Command expresses the intent to record a permanent failure.
type Command struct {
	TransactionID  uuid.UUID
	FailureCode    xerr.Code
	FailureMessage string
	CorrelationID  string
	CausationID    string
}

// AggregateID identifies the WagerTransaction aggregate this command targets.
func (c Command) AggregateID() string { return c.TransactionID.String() }

// CommandType is the routing name of the command.
func (c Command) CommandType() string { return "FailWagerOperation" }

// State is the transaction's position in its lifecycle.
type State struct {
	registered   bool
	status       domain.Status
	kind         domain.Kind
	money        money.Money
	walletID     uuid.UUID
	playerID     uuid.UUID
	roundID      string
	gameID       string
	providerID   string
	externalID   string
	referenceExt *string
	createdAt    time.Time
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
	case *events.WagerTransactionRejected:
		s.status = domain.StatusRejected
	case *events.WagerTransactionFailed:
		s.status = domain.StatusFailed
	}
	return s
}

// decide records the failure.
func decide(s State, cmd Command) ([]cqrs.Event, error) {
	if !s.registered {
		return nil, xerr.NotFound(xerr.CodeResourceNotFound, "Transaction not registered",
			"Transaction $transaction was never accepted, so there is nothing to fail.",
			map[string]any{"transaction": cmd.TransactionID.String()})
	}
	if cmd.FailureCode == "" {
		return nil, xerr.Infrastructure("failure without a code", nil)
	}

	tx, err := domain.Rehydrate(domain.Snapshot{
		ID:                  cmd.TransactionID,
		Origin:              domain.OriginExternal,
		ProviderID:          s.providerID,
		ExternalID:          s.externalID,
		WalletID:            s.walletID,
		PlayerID:            s.playerID,
		RoundID:             s.roundID,
		GameID:              s.gameID,
		Kind:                s.kind,
		Amount:              s.money,
		ReferenceExternalID: s.referenceExt,
		Status:              s.status,
		CreatedAt:           s.createdAt,
		UpdatedAt:           s.createdAt,
	})
	if err != nil {
		return nil, err
	}

	failed, err := tx.Fail(cmd.FailureCode, cmd.FailureMessage, time.Now().UTC())
	if err != nil {
		return nil, err
	}

	return []cqrs.Event{
		&events.WagerTransactionFailed{
			TransactionID:   cmd.TransactionID,
			ProviderID:      failed.ProviderID(),
			ExternalID:      failed.ExternalID(),
			WalletID:        failed.WalletID(),
			Kind:            failed.Kind(),
			Money:           failed.Amount(),
			FailureCode:     cmd.FailureCode,
			FailureMessage:  cmd.FailureMessage,
			ReferenceExtern: failed.ReferenceExternalID(),
		},
	}, nil
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
