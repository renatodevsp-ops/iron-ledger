// Package domain is the Wallet aggregate: the financial root of the ledger.
//
// The aggregate owns the balance. Nothing outside this package may change it:
// a caller asks for a movement and the wallet either applies it — refusing a
// debit that would overdraw it — or returns a typed error. The zero value is
// not a wallet, and every constructor validates, so an uninitialised or
// impossible wallet can never reach persistence.
package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// Direction is the sign of a ledger entry.
type Direction string

const (
	// DirectionDebit removes value from the wallet.
	DirectionDebit Direction = "DEBIT"
	// DirectionCredit adds value to the wallet.
	DirectionCredit Direction = "CREDIT"
)

// InitialVersion is the version a wallet carries right after it is opened,
// before any movement.
const InitialVersion int64 = 1

// Movement is one requested change to a wallet balance. It is validated by
// Wallet.Apply.
type Movement struct {
	// TransactionID is the internal wager transaction causing the movement.
	// It is what ties the balance change to its ledger entry and guarantees one
	// financial movement per transaction.
	TransactionID uuid.UUID
	Direction     Direction
	Amount        money.Money
}

// Snapshot is the plain projection of a Wallet. It exists so the event stream
// can rebuild an aggregate without exposing its fields, and so a rehydrated
// wallet is built through exactly the same validation as a new one.
type Snapshot struct {
	ID        uuid.UUID
	PlayerID  uuid.UUID
	Currency  money.Currency
	Balance   money.Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Wallet is the aggregate root guarding a player's balance.
type Wallet struct {
	id        uuid.UUID
	playerID  uuid.UUID
	currency  money.Currency
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// Open creates a wallet for a player in a single currency.
//
// A zero initial balance is valid and creates a wallet with nothing to prove;
// a negative one is not. The returned wallet is at InitialVersion.
func Open(id, playerID uuid.UUID, currency money.Currency, initial money.Money, at time.Time) (Wallet, error) {
	if id == uuid.Nil {
		return Wallet{}, xerr.Validation(xerr.CodeInvalidRequest, "Invalid wallet id",
			"$field is required and must be a UUID.", map[string]any{"field": "walletId"})
	}
	if playerID == uuid.Nil {
		return Wallet{}, xerr.Validation(xerr.CodeInvalidRequest, "Invalid player id",
			"$field is required and must be a UUID.", map[string]any{"field": "playerId"})
	}
	if currency == "" {
		return Wallet{}, xerr.Validation(xerr.CodeInvalidRequest, "Invalid currency",
			"$field is required and must be an ISO 4217 code.", map[string]any{"field": "currency"})
	}
	if initial.Currency() != currency {
		return Wallet{}, xerr.Validation(xerr.CodeCurrencyMismatch, "Currency mismatch",
			"$initial must be denominated in $currency, the currency of the wallet.",
			map[string]any{"initial": initial.Currency(), "currency": currency})
	}
	if initial.IsNegative() {
		return Wallet{}, xerr.Validation(xerr.CodeNegativeAmountNotAllowed, "Negative initial balance",
			"$initial must be zero or positive. Open the wallet and credit it with an operation instead.",
			map[string]any{"initial": initial.String()})
	}

	at = at.UTC()
	return Wallet{
		id:        id,
		playerID:  playerID,
		currency:  currency,
		balance:   initial,
		version:   InitialVersion,
		createdAt: at,
		updatedAt: at,
	}, nil
}

// Rehydrate rebuilds a wallet from persisted state.
//
// It validates, because a projection corrupted by a bad migration or a manual
// fix must be caught, not carried forward. It performs no movement, no
// transition and emits no event: rebuilding state is not an event.
func Rehydrate(s Snapshot) (Wallet, error) {
	w, err := Open(s.ID, s.PlayerID, s.Currency, s.Balance, s.CreatedAt)
	if err != nil {
		return Wallet{}, err
	}
	if s.Version < InitialVersion {
		return Wallet{}, xerr.Infrastructure(
			"persisted wallet carries a version below the initial version", nil)
	}
	w.version = s.Version
	w.updatedAt = s.UpdatedAt.UTC()
	return w, nil
}

// ID returns the wallet identity.
func (w Wallet) ID() uuid.UUID { return w.id }

// PlayerID returns the owner of the wallet.
func (w Wallet) PlayerID() uuid.UUID { return w.playerID }

// Currency returns the single currency the wallet settles in.
func (w Wallet) Currency() money.Currency { return w.currency }

// Balance returns the current balance.
func (w Wallet) Balance() money.Money { return w.balance }

// Version returns the wallet version. It is InitialVersion right after opening
// and grows by one on every balance change.
func (w Wallet) Version() int64 { return w.version }

// CreatedAt returns the opening instant.
func (w Wallet) CreatedAt() time.Time { return w.createdAt }

// UpdatedAt returns the instant of the last balance change.
func (w Wallet) UpdatedAt() time.Time { return w.updatedAt }

// Snapshot projects the wallet into plain data.
func (w Wallet) Snapshot() Snapshot {
	return Snapshot{
		ID:        w.id,
		PlayerID:  w.playerID,
		Currency:  w.currency,
		Balance:   w.balance,
		Version:   w.version,
		CreatedAt: w.createdAt,
		UpdatedAt: w.updatedAt,
	}
}

// Apply returns the wallet resulting from movement.
//
// The aggregate, not the caller, decides whether the balance may move: a debit
// that would take the balance below zero is refused, an amount in another
// currency is refused, and a movement carrying no transaction is refused. A
// zero amount is refused too — a movement that changes nothing has no ledger
// entry to write and would leave the wallet version inconsistent with its
// balance.
func (w Wallet) Apply(movement Movement, at time.Time) (Wallet, error) {
	if movement.TransactionID == uuid.Nil {
		return Wallet{}, xerr.Validation(xerr.CodeInvalidRequest, "Missing transaction",
			"$field is required so the movement can be tied to a ledger entry.", map[string]any{"field": "transactionId"})
	}
	if movement.Direction != DirectionDebit && movement.Direction != DirectionCredit {
		return Wallet{}, xerr.Validation(xerr.CodeInvalidRequest, "Invalid direction",
			"$field must be DEBIT or CREDIT.", map[string]any{"field": "direction"})
	}
	if movement.Amount.Currency() != w.currency {
		return Wallet{}, xerr.Validation(xerr.CodeCurrencyMismatch, "Currency mismatch",
			"$amount must be denominated in $currency, the currency of the wallet.",
			map[string]any{"amount": movement.Amount.Currency(), "currency": w.currency})
	}
	if !movement.Amount.IsPositive() {
		return Wallet{}, xerr.Validation(xerr.CodeAmountMustBePositive, "Amount must be positive",
			"$amount must be greater than 0.00. A movement with no value has no ledger entry.",
			map[string]any{"amount": movement.Amount.String()})
	}

	var updated money.Money
	var err error
	switch movement.Direction {
	case DirectionCredit:
		if updated, err = w.balance.Add(movement.Amount); err != nil {
			return Wallet{}, wrapArithmetic(err, w.balance, movement.Amount)
		}
	case DirectionDebit:
		if updated, err = w.balance.Sub(movement.Amount); err != nil {
			return Wallet{}, wrapArithmetic(err, w.balance, movement.Amount)
		}
		if updated.IsNegative() {
			return Wallet{}, xerr.Rejected(xerr.CodeInsufficientBalance, "Insufficient balance",
				"The wallet holds $balance, which does not cover $amount. Credit the wallet before retrying the operation.",
				map[string]any{
					"balance":  w.balance.String(),
					"amount":   movement.Amount.String(),
					"currency": string(w.currency),
				})
		}
	}

	w.balance = updated
	w.version++
	w.updatedAt = at.UTC()
	return w, nil
}

func wrapArithmetic(err error, a, b money.Money) error {
	if e, ok := xerr.As(err); ok {
		return e
	}
	return xerr.Infrastructure(
		fmt.Sprintf("money arithmetic overflow while applying %s to %s", b, a), err)
}
