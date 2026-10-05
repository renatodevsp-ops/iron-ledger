// Package awaitwagerreference is the STATE_CHANGE slice that parks a reversal
// until the operation it reverses has been processed.
//
// A reversal routinely arrives before the operation it reverses: providers
// deliver independently and at-least-once. Refusing it would be wrong, and
// guessing would move money that does not exist. So the operation is recorded
// as PENDING_REFERENCE with a durable retry schedule, and a worker picks it up
// later — in this process or any other.
package awaitwagerreference

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

// Command expresses the intent to wait for a reference before processing.
type Command struct {
	TransactionID uuid.UUID
	NextAttemptAt time.Time
	Reason        string
	CorrelationID string
	CausationID   string
}

// AggregateID identifies the WagerTransaction aggregate this command targets.
func (c Command) AggregateID() string { return c.TransactionID.String() }

// CommandType is the routing name of the command.
func (c Command) CommandType() string { return "AwaitWagerReference" }

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
	// pendingAttempts carries the retry count across reschedules. Without it in
	// the state, every reschedule would restart from zero and the TTL that
	// finally rejects an unresolvable reversal would never be reached.
	pendingAttempts int
	createdAt       time.Time
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
		s.pendingAttempts = event.Attempt
	case *events.WagerTransactionProcessed:
		s.status = domain.StatusProcessed
	case *events.WagerTransactionRejected:
		s.status = domain.StatusRejected
	case *events.WagerTransactionFailed:
		s.status = domain.StatusFailed
	}
	return s
}

// decide parks the transaction and schedules its next attempt.
func decide(s State, cmd Command) ([]cqrs.Event, error) {
	if !s.registered {
		return nil, xerr.NotFound(xerr.CodeResourceNotFound, "Transaction not registered",
			"Transaction $transaction was never accepted, so it cannot wait for a reference.",
			map[string]any{"transaction": cmd.TransactionID.String()})
	}
	if s.referenceExt == nil || *s.referenceExt == "" {
		return nil, xerr.Validation(xerr.CodeReferenceRequired, "Reference required",
			"Transaction $transaction names no reference to wait for.",
			map[string]any{"transaction": cmd.TransactionID.String()})
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
		PendingAttempts:     s.pendingAttempts,
		CreatedAt:           s.createdAt,
		UpdatedAt:           s.createdAt,
	})
	if err != nil {
		return nil, err
	}

	waited, err := tx.AwaitReference(cmd.NextAttemptAt, cmd.Reason, time.Now().UTC())
	if err != nil {
		return nil, err
	}

	return []cqrs.Event{
		&events.WagerTransactionPendingReference{
			TransactionID:   cmd.TransactionID,
			ProviderID:      waited.ProviderID(),
			ExternalID:      waited.ExternalID(),
			WalletID:        waited.WalletID(),
			PlayerID:        waited.PlayerID(),
			RoundID:         waited.RoundID(),
			GameID:          waited.GameID(),
			Kind:            waited.Kind(),
			Money:           waited.Amount(),
			ReferenceExtern: *waited.ReferenceExternalID(),
			Attempt:         waited.PendingAttempts(),
			NextAttemptAt:   cmd.NextAttemptAt.UTC(),
			Reason:          cmd.Reason,
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
