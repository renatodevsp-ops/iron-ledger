// Package registerwageroperation is the STATE_CHANGE slice that accepts an
// external operation.
//
// Stream: the WagerTransaction aggregate, created empty. decide validates the
// reported operation and emits WagerTransactionRegistered. Nothing moves here:
// acceptance and settlement are separate commands, so an operation that is
// accepted and then interrupted is left in PENDING and resumable, never half
// applied.
package registerwageroperation

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

// Command expresses the intent to accept an operation reported by a provider.
type Command struct {
	TransactionID       uuid.UUID
	Origin              domain.Origin
	ProviderID          string
	ExternalID          string
	IdempotencyKey      string
	PayloadHash         string
	WalletID            uuid.UUID
	PlayerID            uuid.UUID
	RoundID             string
	GameID              string
	Kind                domain.Kind
	Money               money.Money
	ReferenceExternalID *string
	CorrelationID       string
	CausationID         string
}

// AggregateID identifies the WagerTransaction aggregate this command targets.
func (c Command) AggregateID() string { return c.TransactionID.String() }

// CommandType is the routing name of the command.
func (c Command) CommandType() string { return "RegisterWagerOperation" }

// State is everything decide needs to know about the transaction stream.
type State struct {
	registered bool
	status     domain.Status
}

func initialState() State { return State{} }

// evolve folds one historical event into the current state.
func evolve(s State, envelope *cqrs.Envelope) State {
	switch envelope.Event.(type) {
	case *events.WagerTransactionRegistered:
		s.registered = true
		s.status = domain.StatusPending
	case *events.WagerTransactionPendingReference:
		s.registered = true
		s.status = domain.StatusPendingReference
	case *events.WagerTransactionProcessed:
		s.registered = true
		s.status = domain.StatusProcessed
	case *events.WagerTransactionRejected:
		s.registered = true
		s.status = domain.StatusRejected
	case *events.WagerTransactionFailed:
		s.registered = true
		s.status = domain.StatusFailed
	}
	return s
}

// decide validates the reported operation and returns the acceptance event.
func decide(s State, cmd Command) ([]cqrs.Event, error) {
	if s.registered {
		return nil, xerr.Conflict(xerr.CodeExternalTransactionConflict, "Transaction already registered",
			"Transaction $transaction is already $status; it cannot be registered again.",
			map[string]any{"transaction": cmd.TransactionID.String(), "status": string(s.status)})
	}

	if _, err := domain.Register(domain.Registration{
		ID:                  cmd.TransactionID,
		Origin:              cmd.Origin,
		ProviderID:          cmd.ProviderID,
		ExternalID:          cmd.ExternalID,
		IdempotencyKey:      cmd.IdempotencyKey,
		PayloadHash:         cmd.PayloadHash,
		WalletID:            cmd.WalletID,
		PlayerID:            cmd.PlayerID,
		RoundID:             cmd.RoundID,
		GameID:              cmd.GameID,
		Kind:                cmd.Kind,
		Amount:              cmd.Money,
		ReferenceExternalID: cmd.ReferenceExternalID,
		RecordedAt:          time.Now().UTC(),
	}, time.Now().UTC()); err != nil {
		return nil, err
	}

	return []cqrs.Event{
		&events.WagerTransactionRegistered{
			TransactionID:   cmd.TransactionID,
			Origin:          cmd.Origin,
			ProviderID:      cmd.ProviderID,
			ExternalID:      cmd.ExternalID,
			IdempotencyKey:  cmd.IdempotencyKey,
			PayloadHash:     cmd.PayloadHash,
			WalletID:        cmd.WalletID,
			PlayerID:        cmd.PlayerID,
			RoundID:         cmd.RoundID,
			GameID:          cmd.GameID,
			Kind:            cmd.Kind,
			Money:           cmd.Money,
			ReferenceExtern: cmd.ReferenceExternalID,
		},
	}, nil
}

// NewCommandHandler builds the handler backing this slice.
func NewCommandHandler(store cqrs.EventStore, extractors ...func(context.Context) map[string]any) cqrs.CommandHandler[Command] {
	options := []cqrs.CommandHandlerOption{
		cqrs.WithStreamState(cqrs.NoStream{}),
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
