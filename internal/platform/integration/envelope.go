// Package integration defines the contract of the events this platform
// publishes to the rest of the company.
//
// Every outbound event is an Envelope: a stable identity, the aggregate it
// belongs to, the correlation and causation chain, a schema version and a typed
// payload. The envelope is rendered once, from a committed event, and stored
// verbatim in the outbox row; the publisher never re-derives it, so a
// republication after a crash carries the very same eventId.
package integration

import (
	"time"

	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"
)

// EventType names an outbound event. The value is the contract: consumers route
// on it.
type EventType string

// The events this platform publishes.
const (
	EventWagerTransactionProcessed  EventType = "WagerTransactionProcessed"
	EventWagerTransactionRejected   EventType = "WagerTransactionRejected"
	EventWagerTransactionPendingRef EventType = "WagerTransactionPendingReference"
	EventWalletBalanceChanged       EventType = "WalletBalanceChanged"
)

// Version is the schema version stamped on every envelope. It is defined by the
// constructor of the event, not by the transport, so a new schema means a new
// constructor.
const Version = 1

// Aggregate types carried by the envelope.
const (
	AggregateWallet           = "wallet"
	AggregateWagerTransaction = "wager-transaction"
)

// Metadata keys the event store stamps on every envelope and the integration
// envelope copies out.
const (
	MetaCorrelationID = "correlationId"
	MetaCausationID   = "causationId"
	MetaTransactionID = "transactionId"
	MetaWalletID      = "walletId"
	MetaProviderID    = "providerId"
	MetaOrigin        = "origin"
)

// Envelope is the wire contract of a published event.
type Envelope struct {
	EventID       uuid.UUID `json:"eventId"`
	EventType     EventType `json:"eventType"`
	AggregateType string    `json:"aggregateType"`
	AggregateID   string    `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   string    `json:"causationId,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	Version       int       `json:"version"`
	Data          any       `json:"data"`
}

// Message is what travels over SQS: the envelope plus the identity of the
// publication, which the publisher keeps stable across retries so the broker's
// FIFO deduplication collapses duplicate attempts.
type Message struct {
	MessageID   string    `json:"messageId"`
	PublishedAt time.Time `json:"publishedAt"`
	Envelope    Envelope  `json:"envelope"`
}

// Builder turns a committed domain event into the envelope published to the
// outside world. A bounded context implements it for its own events; returning
// false means "this event is internal, do not publish it".
type Builder interface {
	Build(source Source) (Envelope, bool)
}

// Source is the committed event an Envelope is built from.
type Source struct {
	EventID       uuid.UUID
	EventType     string
	AggregateID   string
	StreamVersion uint64
	OccurredAt    time.Time
	Metadata      map[string]any
	Event         cqrs.Event
}

// SourceOf adapts a committed envelope to a Source.
func SourceOf(env cqrs.Envelope) Source {
	aggregateID := env.StreamID
	if env.Event != nil {
		aggregateID = env.Event.AggregateID()
	}
	return Source{
		EventID:       env.EventID,
		EventType:     env.Event.EventType(),
		AggregateID:   aggregateID,
		StreamVersion: env.Version,
		OccurredAt:    env.OccurredAt.UTC(),
		Metadata:      env.Metadata,
		Event:         env.Event,
	}
}

// MetadataString reads a string from the event metadata.
func MetadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	v, _ := metadata[key].(string)
	return v
}

// MultiBuilder fans an event out to every registered builder and returns the
// first envelope produced. A bounded context is free to publish nothing.
type MultiBuilder []Builder

// Build implements Builder.
func (m MultiBuilder) Build(source Source) (Envelope, bool) {
	for _, b := range m {
		if envelope, ok := b.Build(source); ok {
			return envelope, true
		}
	}
	return Envelope{}, false
}
