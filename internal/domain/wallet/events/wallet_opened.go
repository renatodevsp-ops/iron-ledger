// Package events holds the events the Wallet aggregate publishes.
//
// They are both the history the aggregate is rebuilt from and the payloads the
// platform publishes to the rest of the company, so there is exactly one
// definition of "the balance changed" in the system.
package events

import (
	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
)

// WalletOpened records the creation of a wallet.
//
// A wallet opened with a zero balance still emits this event — the wallet
// exists — but no balance was recorded, so no WalletBalanceChanged and no
// financial integration event follow it.
type WalletOpened struct {
	WalletID  uuid.UUID
	PlayerID  uuid.UUID
	OpeningTx uuid.UUID
	Balance   money.Money
}

var _ cqrs.Event = (*WalletOpened)(nil)

func init() { cqrs.RegisterEvent(&WalletOpened{}) }

// AggregateID identifies the aggregate the event belongs to.
func (e *WalletOpened) AggregateID() string { return e.WalletID.String() }

// EventType is the routing name of the event.
func (e *WalletOpened) EventType() string { return "WalletOpened" }

// ToEvent rebuilds the event from its persisted JSON form. The event store
// decodes through the registry, so this is only used by tests and tooling.
func (e *WalletOpened) ToEvent() *WalletOpened { return e }
