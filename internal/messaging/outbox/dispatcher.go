package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/platform/integration"
)

// Dispatcher turns the events committed by a transaction into durable outbox
// rows, inside that same transaction.
//
// It is deliberately not a separate step: an event is published because it
// happened, and the record of that decision is written where it cannot be lost.
type Dispatcher struct {
	repo     Repository
	builders integration.MultiBuilder
	now      func() time.Time
}

// NewDispatcher builds the dispatcher over a repository and the builders of
// every bounded context that publishes events.
func NewDispatcher(repo Repository, builders integration.MultiBuilder) *Dispatcher {
	return &Dispatcher{repo: repo, builders: builders, now: func() time.Time { return time.Now().UTC() }}
}

// Dispatch renders every publishable event of the transaction and stores it.
//
// The payload is a snapshot: once written, it is never rebuilt from the event
// stream, so a schema change in a later deploy cannot alter what a pending
// event says. It is rendered once and published verbatim, every time.
func (d *Dispatcher) Dispatch(ctx context.Context, envelopes []cqrs.Envelope) error {
	messages := make([]Message, 0, len(envelopes))
	for _, committed := range envelopes {
		source := integration.SourceOf(committed)
		envelope, publish := d.builders.Build(source)
		if !publish {
			continue
		}
		payload, err := json.Marshal(envelope)
		if err != nil {
			return fmt.Errorf("outbox: render %s: %w", envelope.EventID, err)
		}
		now := d.now()
		messages = append(messages, Message{
			EventID:       envelope.EventID,
			AggregateType: envelope.AggregateType,
			AggregateID:   envelope.AggregateID,
			EventType:     string(envelope.EventType),
			CorrelationID: envelope.CorrelationID,
			CausationID:   envelope.CausationID,
			Payload:       payload,
			OccurredAt:    envelope.OccurredAt,
			AvailableAt:   now,
		})
	}
	if len(messages) == 0 {
		return nil
	}
	return d.repo.Append(ctx, messages)
}
