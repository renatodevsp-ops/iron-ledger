// Package walletledgerentries is the STATE_VIEW slice behind the append-only
// ledger.
//
// Every effective change of a balance produces exactly one row here, written in
// the same commit as the balance itself. The rows are never updated and never
// deleted — a correction is a new pair of entries, not an edit — so the ledger
// is the audit trail the reconciliation reconstructs the balance from.
package walletledgerentries

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/platform/repository"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
)

// TableName is the projection this slice owns.
const TableName = "wallet_ledger_entries"

// EntryEntity is one append-only ledger entry.
//
// The balance columns are captured at write time, so the ledger alone is enough
// to prove what the balance was before and after any entry without replaying
// the event stream.
type EntryEntity struct {
	ID                 uuid.UUID `json:"id"`
	WalletID           uuid.UUID `json:"walletId"`
	TransactionID      uuid.UUID `json:"transactionId"`
	Direction          string    `json:"direction"`
	AmountMinor        int64     `json:"amountMinor"`
	BalanceBeforeMinor int64     `json:"balanceBeforeMinor"`
	BalanceAfterMinor  int64     `json:"balanceAfterMinor"`
	Currency           string    `json:"currency"`
	CreatedAt          time.Time `json:"createdAt"`
}

// Amount returns the signed movement this entry records.
func (e *EntryEntity) Amount() money.Money {
	return money.FromMinor(e.AmountMinor, money.Currency(e.Currency))
}

// BalanceBefore returns the balance this entry started from.
func (e *EntryEntity) BalanceBefore() money.Money {
	return money.FromMinor(e.BalanceBeforeMinor, money.Currency(e.Currency))
}

// BalanceAfter returns the balance this entry left behind.
func (e *EntryEntity) BalanceAfter() money.Money {
	return money.FromMinor(e.BalanceAfterMinor, money.Currency(e.Currency))
}

// Repository is the read side of the ledger. It is deliberately not
// generic: the ledger is append-only, so there is no update or delete to
// offer, and an interface that offered one would be a lie the compiler could
// not catch.
type Repository interface {
	// Find returns a single entry by id.
	Find(ctx context.Context, id string) (*EntryEntity, error)
	// Page returns entries of a wallet, oldest first, starting after cursor.
	Page(ctx context.Context, walletID uuid.UUID, after repository.PageCursor, limit int) (repository.Connection[EntryEntity], error)
	// SumByDirection returns the net movement recorded for a wallet, derived
	// from the ledger alone.
	SumByDirection(ctx context.Context, walletID uuid.UUID) (credits, debits, count int64, err error)
}

// EntryQuery pages the ledger of one wallet.
type EntryQuery struct {
	WalletID uuid.UUID
	Cursor   string
	Limit    int
}

// ID implements cqrs.Query.
func (q EntryQuery) ID() []byte {
	out := append([]byte("walletledgerentries:"), q.WalletID[:]...)
	return append(out, []byte(":"+q.Cursor)...)
}

// EntryReadModel is one page of the ledger.
type EntryReadModel struct {
	Data   []*EntryEntity
	Cursor string
}
