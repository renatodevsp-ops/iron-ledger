// Package worker holds the long-running components of the platform: the SQS
// consumer, the outbox publisher and the pending-reference resolver.
//
// Every worker is a plain Run(ctx) loop owned by the Fx lifecycle. Stopping is
// cooperative and observable: on cancellation a worker stops fetching new work,
// finishes what it has already taken within its budget, and hands back anything
// it could not finish so another instance can pick it up immediately rather than
// after a visibility timeout.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/app/usecase"
	"github.com/ironledger/iron-ledger/internal/messaging/sqs"
	"github.com/ironledger/iron-ledger/internal/platform/config"
	"github.com/ironledger/iron-ledger/internal/platform/logging"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// ConsumerName identifies this consumer in the inbox. It is part of the inbox
// primary key, so the same message may legitimately be handled by two different
// consumers without either of them suppressing the other.
const ConsumerName = "wager-operations"

// Broker is the slice of SQS the consumer needs.
type Broker interface {
	Receive(ctx context.Context, queueURL string, max int32, wait time.Duration) ([]sqs.Inbound, error)
	Delete(ctx context.Context, queueURL, receiptHandle string) error
	Release(ctx context.Context, queueURL, receiptHandle string, delay time.Duration) error
	Ready(ctx context.Context) error
}

// Consumer turns at-least-once delivery into at-most-once processing.
//
// The order of operations is the whole point:
//
//  1. receive;
//  2. run the business transaction, which claims the message identity in the
//     inbox and commits the effect together;
//  3. only then delete the message.
//
// A crash between 2 and 3 leaves the message to be redelivered, and the inbox
// turns that redelivery into a replay of the stored outcome rather than a second
// movement. A crash between 1 and 2 simply redelivers. A confirmed business
// rejection is terminal, so the message is deleted rather than retried forever.
type Consumer struct {
	client   Broker
	wagering *usecase.WageringUseCase
	cfg      config.SQS
	logger   *slog.Logger
	metrics  *metrics.Metrics
	instance string

	mu       sync.Mutex
	inFlight map[string]string // receipt handle -> message id
}

// NewConsumer builds the SQS consumer.
func NewConsumer(
	client Broker,
	wagering *usecase.WageringUseCase,
	cfg config.SQS,
	logger *slog.Logger,
	m *metrics.Metrics,
	instance string,
) *Consumer {
	return &Consumer{
		client:   client,
		wagering: wagering,
		cfg:      cfg,
		logger:   logger,
		metrics:  m,
		instance: instance,
		inFlight: make(map[string]string),
	}
}

// Run consumes until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	if c.cfg.QueueURL == "" {
		return errors.New("worker: consumer requires SQS_WAGER_QUEUE_URL")
	}
	c.setRunning(true)
	defer c.setRunning(false)

	c.logger.Info("wager operation consumer started",
		"queue", c.cfg.QueueURL,
		"consumer", ConsumerName,
		"visibilityTimeout", c.cfg.VisibilityTimeout.String(),
	)

	backoff := 200 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			c.releaseInFlight(ctx)
			return nil
		}

		messages, err := c.client.Receive(ctx, c.cfg.QueueURL, c.cfg.MaxMessages, c.cfg.WaitTime)
		if err != nil {
			if ctx.Err() != nil {
				c.releaseInFlight(ctx)
				return nil
			}
			c.logger.Warn("receive failed, retrying", "error", err.Error(), "backoff", backoff.String())
			if !sleep(ctx, backoff) {
				c.releaseInFlight(ctx)
				return nil
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = 200 * time.Millisecond

		if len(messages) == 0 {
			continue
		}

		var wg sync.WaitGroup
		for _, message := range messages {
			wg.Add(1)
			go func(message sqs.Inbound) {
				defer wg.Done()
				c.handle(ctx, message)
			}(message)
		}
		wg.Wait()
	}
}

// handle processes one message.
func (c *Consumer) handle(ctx context.Context, message sqs.Inbound) {
	ctx = logging.WithCorrelationID(ctx, "")
	ctx = logging.WithMessageID(ctx, message.MessageID)

	c.track(message)
	defer c.untrack(message)

	envelope, err := decodeEnvelope(message.Body)
	if err != nil {
		// A message this build cannot parse is never going to parse. It is left
		// on the queue so the redrive policy sends it to the dead-letter queue
		// where it can be inspected, rather than being silently discarded.
		c.logger.Error("unreadable message left for the dead-letter queue",
			append([]any{"error", err.Error(), "receiveCount", message.ReceiveCount},
				uow.LogFields(ctx)...)...)
		c.count("inbox_failures", "malformed")
		return
	}

	if envelope.Type != envelopeTypeRequested {
		c.logger.Error("unexpected message type left for the dead-letter queue",
			append([]any{"type", envelope.Type}, uow.LogFields(ctx)...)...)
		c.count("inbox_failures", "unexpected_type")
		return
	}

	playerID, err := uuid.Parse(envelope.Data.PlayerID)
	if err != nil {
		c.logger.Error("message carries an invalid playerId; left for the dead-letter queue",
			append([]any{"playerId", envelope.Data.PlayerID}, uow.LogFields(ctx)...)...)
		c.count("inbox_failures", "malformed")
		return
	}
	walletID, err := uuid.Parse(envelope.Data.WalletID)
	if err != nil {
		c.logger.Error("message carries an invalid walletId; left for the dead-letter queue",
			append([]any{"walletId", envelope.Data.WalletID}, uow.LogFields(ctx)...)...)
		c.count("inbox_failures", "malformed")
		return
	}

	ctx = logging.WithProviderID(ctx, envelope.Data.ProviderID)
	ctx = logging.WithWalletID(ctx, envelope.Data.WalletID)

	result, err := c.wagering.Submit(ctx, usecase.OperationRequest{
		IdempotencyKey:        envelope.Data.IdempotencyKey,
		ProviderID:            envelope.Data.ProviderID,
		ExternalTransactionID: envelope.Data.ExternalTransactionID,
		PlayerID:              playerID,
		WalletID:              walletID,
		RoundID:               envelope.Data.RoundID,
		GameID:                envelope.Data.GameID,
		Kind:                  envelope.Data.Kind,
		Amount:                envelope.Data.Money.Amount,
		Currency:              envelope.Data.Money.Currency,
		ReferenceExternalID:   optional(envelope.Data.ReferenceExternalTransactionID),
		Source:                usecase.SourceSQS,
		Inbox: &usecase.InboxContext{
			ConsumerName: ConsumerName,
			MessageID:    envelope.MessageID,
			PayloadHash:  hashOf(message.Body),
			ReceivedAt:   time.Now().UTC(),
		},
	})

	if err != nil {
		c.handleFailure(ctx, message, err)
		return
	}

	c.logger.Info("operation processed",
		append([]any{
			"status", result.Status,
			"replay", result.IdempotentReplay,
			"transactionId", result.TransactionID.String(),
		}, uow.LogFields(ctx)...)...)

	if err := c.client.Delete(ctx, c.cfg.QueueURL, message.ReceiptHandle); err != nil {
		// The work is committed. The message will be redelivered and the inbox
		// will recognise it, so this is logged and not treated as a failure of
		// the operation itself.
		c.logger.Warn("committed but message not deleted; it will be redelivered",
			append([]any{"error", err.Error()}, uow.LogFields(ctx)...)...)
	}
}

// handleFailure decides whether a failed message is retried or retired.
func (c *Consumer) handleFailure(ctx context.Context, message sqs.Inbound, err error) {
	if xerr.IsTerminal(err) {
		// A confirmed business decision — a rejection with a stable code, a
		// conflict, a bad payload — will never succeed on redelivery, and the
		// operation is already recorded as REJECTED where it should be.
		c.logger.Warn("terminal rejection; message retired",
			append([]any{"error", err.Error()}, uow.LogFields(ctx)...)...)
		c.count("inbox_completed", "rejected")
		if delErr := c.client.Delete(ctx, c.cfg.QueueURL, message.ReceiptHandle); delErr != nil {
			c.logger.Warn("could not retire a rejected message",
				append([]any{"error", delErr.Error()}, uow.LogFields(ctx)...)...)
		}
		return
	}

	// A transient failure: the operation either never committed or rolled back,
	// so it is safe to try again. The broker redrives it, and the attempt count
	// eventually sends it to the dead-letter queue.
	c.logger.Warn("transient failure; message left for redelivery",
		append([]any{"error", err.Error(), "receiveCount", message.ReceiveCount}, uow.LogFields(ctx)...)...)
	c.count("inbox_failures", "transient")
}

// envelope mirrors the broker contract of an inbound message.
const envelopeTypeRequested = "WagerTransactionRequested"

type inboundEnvelope struct {
	MessageID  string    `json:"messageId"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurredAt"`
	Data       struct {
		ProviderID            string `json:"providerId"`
		ExternalTransactionID string `json:"externalTransactionId"`
		IdempotencyKey        string `json:"idempotencyKey"`
		PlayerID              string `json:"playerId"`
		WalletID              string `json:"walletId"`
		RoundID               string `json:"roundId"`
		GameID                string `json:"gameId"`
		Kind                  string `json:"kind"`
		Money                 struct {
			Amount   string `json:"amount"`
			Currency string `json:"currency"`
		} `json:"money"`
		ReferenceExternalTransactionID string `json:"referenceExternalTransactionId"`
	} `json:"data"`
}

func decodeEnvelope(body string) (*inboundEnvelope, error) {
	envelope := &inboundEnvelope{}
	decoder := json.NewDecoder(strings.NewReader(body))
	if err := decoder.Decode(envelope); err != nil {
		return nil, fmt.Errorf("worker: decode envelope: %w", err)
	}
	if envelope.MessageID == "" {
		return nil, errors.New("worker: envelope has no messageId")
	}
	return envelope, nil
}

func (c *Consumer) track(message sqs.Inbound) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight[message.ReceiptHandle] = message.MessageID
}

func (c *Consumer) untrack(message sqs.Inbound) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inFlight, message.ReceiptHandle)
}

// releaseInFlight hands back every message still being processed, so another
// instance can take over immediately instead of waiting out the visibility
// timeout.
func (c *Consumer) releaseInFlight(ctx context.Context) {
	c.mu.Lock()
	handles := make([]string, 0, len(c.inFlight))
	for handle := range c.inFlight {
		handles = append(handles, handle)
	}
	c.inFlight = make(map[string]string)
	c.mu.Unlock()

	if len(handles) == 0 {
		return
	}
	// The context is already cancelled, so the release needs its own deadline.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for _, handle := range handles {
		if err := c.client.Release(releaseCtx, c.cfg.QueueURL, handle, 0); err != nil {
			c.logger.Warn("could not release an in-flight message",
				"error", err.Error(), "messageId", c.instance)
		}
	}
	c.logger.Info("released in-flight messages", "count", len(handles))
}

func (c *Consumer) setRunning(running bool) {
	if c.metrics == nil {
		return
	}
	if running {
		c.metrics.WorkerRunning.WithLabelValues("wager_consumer").Set(1)
	} else {
		c.metrics.WorkerRunning.WithLabelValues("wager_consumer").Set(0)
	}
}

func (c *Consumer) count(metric, label string) {
	if c.metrics == nil {
		return
	}
	switch metric {
	case "inbox_failures":
		c.metrics.InboxFailures.WithLabelValues(ConsumerName, label).Inc()
	case "inbox_completed":
		c.metrics.InboxCompleted.WithLabelValues(ConsumerName, label).Inc()
	}
}

func hashOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > 5*time.Second {
		return 5 * time.Second
	}
	return next
}
