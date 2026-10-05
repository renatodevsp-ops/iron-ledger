// Package pipeline projects committed events and hands them to the outbox,
// inside the transaction that produced them.
//
// It is the seam that makes "publication never precedes its commit" true by
// construction: every event a transaction appends is rendered into an outbox
// row by the same commit, and every projection is written by that same commit,
// so an event cannot be visible while its effects are not, nor the reverse.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/messaging/outbox"
	"github.com/ironledger/iron-ledger/internal/platform/logging"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
)

// Pipeline applies events to the read models and enqueues them for publication.
type Pipeline struct {
	projectors []*cqrs.EventGroupProcessor
	dispatcher *outbox.Dispatcher
	logger     *slog.Logger
}

// New builds the pipeline. The order of projectors matters: a projector that
// writes a row another one references must run first, which is why the wallet
// projections precede the wagering ones.
func New(logger *slog.Logger, dispatcher *outbox.Dispatcher, projectors ...*cqrs.EventGroupProcessor) *Pipeline {
	return &Pipeline{projectors: projectors, dispatcher: dispatcher, logger: logger}
}

// Flush projects and enqueues every event appended since the last flush.
//
// It may be called as often as the caller likes: each envelope is drained
// exactly once, and the loop keeps going until the transaction appends nothing
// new, so a projection that itself appends an event cannot be left behind.
func (p *Pipeline) Flush(ctx context.Context) error {
	recorder := uow.RecorderFromContext(ctx)
	for {
		batch := recorder.Drain()
		if len(batch) == 0 {
			return nil
		}
		for i := range batch {
			envelope := batch[i]
			if err := p.project(ctx, envelope); err != nil {
				return fmt.Errorf("pipeline: project %s: %w", envelope.Event.EventType(), err)
			}
		}
		if p.dispatcher != nil {
			if err := p.dispatcher.Dispatch(ctx, batch); err != nil {
				return fmt.Errorf("pipeline: dispatch: %w", err)
			}
		}
	}
}

func (p *Pipeline) project(ctx context.Context, envelope cqrs.Envelope) error {
	eventCtx := cqrs.WithEnvelope(ctx, &envelope)
	for _, projector := range p.projectors {
		if err := projector.Handle(eventCtx, envelope.Event); err != nil {
			var skipped *cqrs.SkippedEventError
			if errors.As(err, &skipped) {
				continue
			}
			return err
		}
	}
	if p.logger != nil {
		p.logger.Debug("event projected",
			append([]any{
				"eventType", envelope.Event.EventType(),
				"eventId", envelope.EventID.String(),
				"stream", envelope.StreamID,
				"version", envelope.Version,
			}, loggingFields(eventCtx)...)...)
	}
	return nil
}

func loggingFields(ctx context.Context) []any {
	fields := make([]any, 0, 10)
	for _, pair := range [][2]string{
		{logging.FieldCorrelationID, logging.CorrelationID(ctx)},
		{logging.FieldTransactionID, logging.TransactionID(ctx)},
		{logging.FieldWalletID, logging.WalletID(ctx)},
		{logging.FieldProviderID, logging.ProviderID(ctx)},
	} {
		if pair[1] != "" {
			fields = append(fields, pair[0], pair[1])
		}
	}
	return fields
}
