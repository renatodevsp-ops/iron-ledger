// Package events holds the events the WagerTransaction aggregate publishes.
//
// WagerTransactionProcessed, WagerTransactionRejected and
// WagerTransactionPendingReference are part of the platform's external
// contract. WagerTransactionRegistered is internal: it records that the
// platform accepted an operation, which is a fact about its own bookkeeping.
package events

import (
	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
)

// WagerTransactionRegistered records the acceptance of an operation. The
// transaction is PENDING: accepted, not yet settled.
//
// The idempotency key and the payload hash are carried on the event, so the
// stream itself records which delivery it belongs to and what the business
// payload was.
type WagerTransactionRegistered struct {
	TransactionID   uuid.UUID
	Origin          domain.Origin
	ProviderID      string
	ExternalID      string
	IdempotencyKey  string
	PayloadHash     string
	WalletID        uuid.UUID
	PlayerID        uuid.UUID
	RoundID         string
	GameID          string
	Kind            domain.Kind
	Money           money.Money
	ReferenceExtern *string
}

var _ cqrs.Event = (*WagerTransactionRegistered)(nil)

func init() { cqrs.RegisterEvent(&WagerTransactionRegistered{}) }

// AggregateID identifies the aggregate the event belongs to.
func (e *WagerTransactionRegistered) AggregateID() string { return e.TransactionID.String() }

// EventType is the routing name of the event.
func (e *WagerTransactionRegistered) EventType() string { return "WagerTransactionRegistered" }
