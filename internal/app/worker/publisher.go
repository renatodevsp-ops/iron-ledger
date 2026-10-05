package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/messaging/outbox"
	"github.com/ironledger/iron-ledger/internal/platform/config"
	"github.com/ironledger/iron-ledger/internal/platform/integration"
	"github.com/ironledger/iron-ledger/internal/platform/logging"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
)

// EventPublisher is the transport half of the broker, kept separate from
// Receive/Delete so a test can drive publication without a consumer.
type EventPublisher interface {
	Send(ctx context.Context, queueURL string, message integration.Message, groupID, dedupID string) error
	Ready(ctx context.Context) error
}

// Publisher drains the outbox.
//
// Several publishers may run against the same table: each claims a disjoint
// batch with FOR UPDATE SKIP LOCKED, so they never contend. A publisher that
// dies after the broker accepted a send but before it recorded the fact leaves
// the row claimed; once the visibility window elapses another publisher takes it
// and sends it again with the same eventId, which the FIFO queue's
// deduplication collapses. Consumers therefore see each event once even though
// publication is at-least-once.
type Publisher struct {
	client   EventPublisher
	outbox   outbox.Repository
	tx       *uow.Manager
	cfg      config.Outbox
	sqsQueue string
	logger   *slog.Logger
	metrics  *metrics.Metrics
	instance string

	published atomic.Int64
}

// NewPublisher builds the outbox publisher.
func NewPublisher(
	client EventPublisher,
	repo outbox.Repository,
	tx *uow.Manager,
	cfg config.Outbox,
	queueURL string,
	logger *slog.Logger,
	m *metrics.Metrics,
	instance string,
) *Publisher {
	return &Publisher{
		client:   client,
		outbox:   repo,
		tx:       tx,
		cfg:      cfg,
		sqsQueue: queueURL,
		logger:   logger,
		metrics:  m,
		instance: instance,
	}
}

// Run publishes until ctx is cancelled.
func (p *Publisher) Run(ctx context.Context) error {
	if p.sqsQueue == "" {
		return errors.New("worker: publisher requires SQS_EVENTS_QUEUE_URL")
	}
	p.setRunning(true)
	defer p.setRunning(false)

	p.logger.Info("outbox publisher started",
		"queue", p.sqsQueue,
		"batchSize", p.cfg.BatchSize,
		"visibilityWindow", p.cfg.VisibilityWindow.String(),
	)

	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("outbox publisher stopping", "published", p.published.Load())
			return nil
		case <-ticker.C:
			if err := p.drain(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				p.logger.Warn("outbox sweep failed", "error", err.Error())
			}
		}
	}
}

// drain publishes one batch.
func (p *Publisher) drain(ctx context.Context) error {
	messages, err := p.claim(ctx)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		return nil
	}

	var published []uuid.UUID
	for _, message := range messages {
		if err := p.publishOne(ctx, message); err != nil {
			if rescheduleErr := p.reschedule(ctx, message, err); rescheduleErr != nil {
				p.logger.Error("could not reschedule a failed event",
					"eventId", message.EventID.String(),
					"error", rescheduleErr.Error())
			}
			continue
		}
		published = append(published, message.EventID)
		p.published.Add(1)
	}

	if len(published) == 0 {
		return nil
	}
	return p.markPublished(ctx, published)
}

func (p *Publisher) claim(ctx context.Context) ([]outbox.Message, error) {
	var claimed []outbox.Message
	err := p.tx.Do(ctx, uow.Options{Name: "outbox_claim", NoRetry: true}, func(ctx context.Context) error {
		var err error
		claimed, err = p.outbox.Claim(ctx, p.instance, p.cfg.BatchSize, p.cfg.VisibilityWindow)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("worker: claim outbox: %w", err)
	}
	return claimed, nil
}

// publishOne hands one event to the broker.
func (p *Publisher) publishOne(ctx context.Context, message outbox.Message) error {
	transport, err := message.Transport(message.EventID.String(), time.Now().UTC())
	if err != nil {
		// An unreadable payload will never become readable. Retrying forever
		// would block nothing else — claims are disjoint — but it would grow the
		// backlog silently, so it is reported as a permanent failure.
		return permanent(err)
	}

	eventCtx := logging.WithCorrelationID(ctx, message.CorrelationID)
	if err := p.client.Send(eventCtx, p.sqsQueue, transport, message.AggregateID, message.EventID.String()); err != nil {
		return err
	}

	if p.metrics != nil {
		p.metrics.OutboxPublished.WithLabelValues(message.EventType).Inc()
		p.metrics.OutboxLag.Observe(time.Since(message.OccurredAt).Seconds())
	}
	p.logger.Info("event published",
		append([]any{
			"eventId", message.EventID.String(),
			"eventType", message.EventType,
			"aggregateId", message.AggregateID,
			"attempt", message.Attempts,
			"lagMs", time.Since(message.OccurredAt).Milliseconds(),
		}, uow.LogFields(eventCtx)...)...)
	return nil
}

func (p *Publisher) markPublished(ctx context.Context, eventIDs []uuid.UUID) error {
	return p.tx.Do(ctx, uow.Options{Name: "outbox_mark_published", NoRetry: true}, func(ctx context.Context) error {
		return p.outbox.MarkPublished(ctx, eventIDs, time.Now().UTC())
	})
}

func (p *Publisher) reschedule(ctx context.Context, message outbox.Message, cause error) error {
	var permanentFailure *permanentError
	if errors.As(cause, &permanentFailure) {
		p.logger.Error("event cannot be published; dropped from the outbox",
			"eventId", message.EventID.String(),
			"eventType", message.EventType,
			"error", cause.Error())
		if p.metrics != nil {
			p.metrics.DLQMessages.WithLabelValues("outbox").Inc()
		}
		// The event is recorded as delivered so the backlog does not grow, and
		// the operator has an error to act on. The ledger is untouched: the
		// event described a commit that already happened.
		return p.markPublished(ctx, []uuid.UUID{message.EventID})
	}

	if p.metrics != nil {
		p.metrics.OutboxRetries.WithLabelValues(message.EventType).Inc()
	}
	delay := p.backoff(message.Attempts)
	p.logger.Warn("publication failed; rescheduled",
		"eventId", message.EventID.String(),
		"eventType", message.EventType,
		"attempt", message.Attempts,
		"retryIn", delay.String(),
		"error", cause.Error())

	if message.Attempts >= p.cfg.MaxAttempts {
		p.logger.Error("event exhausted its publication budget; dropping it",
			"eventId", message.EventID.String(),
			"eventType", message.EventType,
			"attempts", message.Attempts)
		if p.metrics != nil {
			p.metrics.DLQMessages.WithLabelValues("outbox").Inc()
		}
		return p.markPublished(ctx, []uuid.UUID{message.EventID})
	}

	return p.tx.Do(ctx, uow.Options{Name: "outbox_reschedule", NoRetry: true}, func(ctx context.Context) error {
		return p.outbox.Reschedule(ctx, message.EventID, time.Now().UTC().Add(delay), cause.Error())
	})
}

func (p *Publisher) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := p.cfg.BackoffBase
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= p.cfg.BackoffMax {
			return p.cfg.BackoffMax
		}
	}
	if delay > p.cfg.BackoffMax {
		return p.cfg.BackoffMax
	}
	return delay
}

func (p *Publisher) setRunning(running bool) {
	if p.metrics == nil {
		return
	}
	if running {
		p.metrics.WorkerRunning.WithLabelValues("outbox_publisher").Set(1)
	} else {
		p.metrics.WorkerRunning.WithLabelValues("outbox_publisher").Set(0)
	}
}

// permanentError marks a failure that retrying cannot fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return &permanentError{err: err} }
