// Package wagertransactiondetails is the STATE_VIEW slice behind the wager
// transaction index.
//
// The index is what makes idempotency survive a restart: it is keyed by the
// provider's idempotency key and by the provider's own transaction identity,
// both enforced by unique indexes, and it stores the result the provider was
// given, so a replay reports the original outcome instead of moving money
// again.
package wagertransactiondetails

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/platform/repository"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
)

// TableName is the projection this slice owns.
const TableName = "wager_transactions"

// Entity is the projected transaction row.
type Entity struct {
	ID                uuid.UUID  `json:"id"`
	Origin            string     `json:"origin"`
	ProviderID        string     `json:"providerId"`
	ExternalID        string     `json:"externalTransactionId"`
	IdempotencyKey    string     `json:"idempotencyKey"`
	PayloadHash       string     `json:"payloadHash"`
	WalletID          uuid.UUID  `json:"walletId"`
	PlayerID          uuid.UUID  `json:"playerId"`
	RoundID           string     `json:"roundId"`
	GameID            string     `json:"gameId"`
	Kind              string     `json:"kind"`
	AmountMinor       int64      `json:"amountMinor"`
	Currency          string     `json:"currency"`
	ReferenceExternal string     `json:"referenceExternalTransactionId"`
	ReferenceInternal *uuid.UUID `json:"referenceTransactionId"`
	Status            string     `json:"status"`
	FailureCode       string     `json:"failureCode"`
	FailureMessage    string     `json:"failureMessage"`
	BalanceAfterMinor *int64     `json:"balanceAfterMinor"`
	BalanceAfterCur   string     `json:"balanceAfterCurrency"`
	PendingAttempts   int        `json:"pendingAttempts"`
	NextAttemptAt     *time.Time `json:"nextAttemptAt"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

// Amount returns the operation amount.
func (e *Entity) Amount() money.Money {
	return money.FromMinor(e.AmountMinor, money.Currency(e.Currency))
}

// BalanceAfter returns the balance observed at processing time, if the
// operation was processed.
func (e *Entity) BalanceAfter() *money.Money {
	if e.BalanceAfterMinor == nil {
		return nil
	}
	value := money.FromMinor(*e.BalanceAfterMinor, money.Currency(e.BalanceAfterCur))
	return &value
}

// Kind returns the operation kind.
func (e *Entity) OperationKind() domain.Kind { return domain.Kind(e.Kind) }

// Status returns the lifecycle position.
func (e *Entity) LifecycleStatus() domain.Status { return domain.Status(e.Status) }

// Repository is the projection a projector writes and a query handler reads.
type Repository interface {
	repository.Repository[Entity]
	// FindByIdempotencyKey resolves the transaction a provider already sent
	// under a key.
	FindByIdempotencyKey(ctx context.Context, providerID, key string) (*Entity, error)
	// FindByExternalID resolves the transaction a provider already sent under
	// its own identity.
	FindByExternalID(ctx context.Context, providerID, externalID string) (*Entity, error)
	// FindReversalOf returns the successful reversal of a given operation, if
	// one exists. It is how a second reversal is refused before any money is
	// touched; the partial unique index behind it is what makes that true
	// whatever the code path.
	FindReversalOf(ctx context.Context, referenceID uuid.UUID) (*Entity, error)
	// DuePending returns transactions waiting for a reference whose next
	// attempt is due, oldest first.
	DuePending(ctx context.Context, limit int) ([]*Entity, error)
}

// Query looks a transaction up by its internal identity.
type Query struct {
	TransactionID uuid.UUID
}

// ID implements cqrs.Query.
func (q Query) ID() []byte { return append([]byte("wagertransactiondetails:"), q.TransactionID[:]...) }

// ReadModel is the result of a transaction lookup.
type ReadModel struct {
	Data *Entity
}
