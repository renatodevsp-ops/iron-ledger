package events

import (
	"time"

	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// WagerTransactionProcessed records the successful settlement of an operation,
// including a LOSS, which succeeds without moving money.
//
// BalanceAfter is the balance observed at the moment of settlement. It is kept
// on the transaction, not read from the wallet at replay time, so a replay
// reports the outcome the provider saw even after the wallet has moved on.
type WagerTransactionProcessed struct {
	TransactionID     uuid.UUID
	ProviderID        string
	ExternalID        string
	WalletID          uuid.UUID
	PlayerID          uuid.UUID
	RoundID           string
	GameID            string
	Kind              domain.Kind
	Money             money.Money
	BalanceAfter      money.Money
	ResolvedReference *uuid.UUID
	ReferenceExtern   *string
}

var _ cqrs.Event = (*WagerTransactionProcessed)(nil)

func init() { cqrs.RegisterEvent(&WagerTransactionProcessed{}) }

// AggregateID identifies the aggregate the event belongs to.
func (e *WagerTransactionProcessed) AggregateID() string { return e.TransactionID.String() }

// EventType is the routing name of the event.
func (e *WagerTransactionProcessed) EventType() string { return "WagerTransactionProcessed" }

// WagerTransactionRejected records a terminal business rejection. Nothing
// moved; the reason is stable so a provider can react to it programmatically.
type WagerTransactionRejected struct {
	TransactionID   uuid.UUID
	ProviderID      string
	ExternalID      string
	WalletID        uuid.UUID
	PlayerID        uuid.UUID
	RoundID         string
	GameID          string
	Kind            domain.Kind
	Money           money.Money
	FailureCode     xerr.Code
	FailureMessage  string
	ReferenceExtern *string
}

var _ cqrs.Event = (*WagerTransactionRejected)(nil)

func init() { cqrs.RegisterEvent(&WagerTransactionRejected{}) }

// AggregateID identifies the aggregate the event belongs to.
func (e *WagerTransactionRejected) AggregateID() string { return e.TransactionID.String() }

// EventType is the routing name of the event.
func (e *WagerTransactionRejected) EventType() string { return "WagerTransactionRejected" }

// WagerTransactionPendingReference records that processing is waiting for a
// reference. It is not a failure: the operation is durable and a worker will
// pick it up, retrying with backoff until it succeeds or the retry budget is
// exhausted.
type WagerTransactionPendingReference struct {
	TransactionID   uuid.UUID
	ProviderID      string
	ExternalID      string
	WalletID        uuid.UUID
	PlayerID        uuid.UUID
	RoundID         string
	GameID          string
	Kind            domain.Kind
	Money           money.Money
	ReferenceExtern string
	Attempt         int
	NextAttemptAt   time.Time
	Reason          string
}

var _ cqrs.Event = (*WagerTransactionPendingReference)(nil)

func init() { cqrs.RegisterEvent(&WagerTransactionPendingReference{}) }

// AggregateID identifies the aggregate the event belongs to.
func (e *WagerTransactionPendingReference) AggregateID() string { return e.TransactionID.String() }

// EventType is the routing name of the event.
func (e *WagerTransactionPendingReference) EventType() string {
	return "WagerTransactionPendingReference"
}

// WagerTransactionFailed records a permanent infrastructure failure. It exists
// so a transaction that cannot be processed is never silently left in PENDING:
// the attempt is recorded and the operator can see it.
type WagerTransactionFailed struct {
	TransactionID   uuid.UUID
	ProviderID      string
	ExternalID      string
	WalletID        uuid.UUID
	Kind            domain.Kind
	Money           money.Money
	FailureCode     xerr.Code
	FailureMessage  string
	ReferenceExtern *string
}

var _ cqrs.Event = (*WagerTransactionFailed)(nil)

func init() { cqrs.RegisterEvent(&WagerTransactionFailed{}) }

// AggregateID identifies the aggregate the event belongs to.
func (e *WagerTransactionFailed) AggregateID() string { return e.TransactionID.String() }

// EventType is the routing name of the event.
func (e *WagerTransactionFailed) EventType() string { return "WagerTransactionFailed" }
