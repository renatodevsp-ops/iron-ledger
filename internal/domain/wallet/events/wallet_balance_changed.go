package events

import (
	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/domain/wallet/domain"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
)

// WalletBalanceChanged records one effective change of a wallet balance. It is
// always accompanied by exactly one ledger entry, committed in the same
// transaction, and the two agree by construction: BalanceAfter is
// BalanceBefore ± Money according to Direction.
type WalletBalanceChanged struct {
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     domain.Direction
	Money         money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	WalletVersion int64
}

var _ cqrs.Event = (*WalletBalanceChanged)(nil)

func init() { cqrs.RegisterEvent(&WalletBalanceChanged{}) }

// AggregateID identifies the aggregate the event belongs to.
func (e *WalletBalanceChanged) AggregateID() string { return e.WalletID.String() }

// EventType is the routing name of the event.
func (e *WalletBalanceChanged) EventType() string { return "WalletBalanceChanged" }
