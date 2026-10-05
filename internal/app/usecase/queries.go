package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/wagertransactiondetails"
	"github.com/ironledger/iron-ledger/internal/platform/repository"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// TransactionView is the wire form of a wager transaction.
//
// It reports the balance observed when the operation was processed, not the
// wallet's current balance, so a provider polling a transaction days later sees
// the answer it was given the first time.
type TransactionView struct {
	TransactionID     string     `json:"transactionId"`
	ProviderID        string     `json:"providerId,omitempty"`
	ExternalID        string     `json:"externalTransactionId,omitempty"`
	WalletID          string     `json:"walletId"`
	PlayerID          string     `json:"playerId,omitempty"`
	RoundID           string     `json:"roundId,omitempty"`
	GameID            string     `json:"gameId,omitempty"`
	Kind              string     `json:"kind"`
	Money             *MoneyView `json:"money"`
	Status            string     `json:"status"`
	Balance           *MoneyView `json:"balance,omitempty"`
	FailureCode       string     `json:"failureCode,omitempty"`
	FailureMessage    string     `json:"failureMessage,omitempty"`
	ReferenceExternal string     `json:"referenceExternalTransactionId,omitempty"`
	ReferenceInternal string     `json:"referenceTransactionId,omitempty"`
	PendingAttempts   int        `json:"pendingAttempts,omitempty"`
	CreatedAt         string     `json:"createdAt"`
	UpdatedAt         string     `json:"updatedAt"`
}

// Queries reads transactions. It is separate from WageringUseCase so the read
// side has no ability to move money.
type Queries struct {
	transactions TransactionReader
}

// NewQueries builds the read side.
func NewQueries(transactions TransactionReader) *Queries {
	return &Queries{transactions: transactions}
}

// GetTransaction returns a transaction by its internal identity.
func (q *Queries) GetTransaction(ctx context.Context, transactionID uuid.UUID) (*TransactionView, error) {
	entity, err := q.transactions.Find(ctx, transactionID.String())
	if err != nil {
		if isNotFound(err) {
			return nil, xerr.NotFound(xerr.CodeResourceNotFound, "Transaction not found",
				"Transaction $transaction does not exist.",
				map[string]any{"transaction": transactionID.String()})
		}
		return nil, unavailable(err)
	}
	return viewOfTransaction(entity), nil
}

// GetProviderTransaction returns a transaction by the provider's own identity,
// always scoped to a provider: there is no unscope lookup to get wrong.
func (q *Queries) GetProviderTransaction(ctx context.Context, providerID, externalID string) (*TransactionView, error) {
	entity, err := q.transactions.FindByExternalID(ctx, providerID, externalID)
	if err != nil {
		if isNotFound(err) {
			return nil, xerr.NotFound(xerr.CodeResourceNotFound, "Transaction not found",
				"Transaction $external does not exist for this provider.",
				map[string]any{"external": externalID})
		}
		return nil, unavailable(err)
	}
	return viewOfTransaction(entity), nil
}

// OwnerProviderOf returns the provider a transaction belongs to, so a caller
// can be checked against it before the transaction is revealed.
func (q *Queries) OwnerProviderOf(ctx context.Context, transactionID uuid.UUID) (string, error) {
	entity, err := q.transactions.Find(ctx, transactionID.String())
	if err != nil {
		if isNotFound(err) {
			return "", xerr.NotFound(xerr.CodeResourceNotFound, "Transaction not found",
				"Transaction $transaction does not exist.",
				map[string]any{"transaction": transactionID.String()})
		}
		return "", unavailable(err)
	}
	return entity.ProviderID, nil
}

func viewOfTransaction(entity *wagertransactiondetails.Entity) *TransactionView {
	amount := entity.Amount()
	view := &TransactionView{
		TransactionID:     entity.ID.String(),
		ProviderID:        entity.ProviderID,
		ExternalID:        entity.ExternalID,
		WalletID:          entity.WalletID.String(),
		PlayerID:          entity.PlayerID.String(),
		RoundID:           entity.RoundID,
		GameID:            entity.GameID,
		Kind:              entity.Kind,
		Money:             moneyView(&amount),
		Status:            entity.Status,
		Balance:           moneyView(entity.BalanceAfter()),
		FailureCode:       entity.FailureCode,
		FailureMessage:    entity.FailureMessage,
		ReferenceExternal: entity.ReferenceExternal,
		PendingAttempts:   entity.PendingAttempts,
		CreatedAt:         entity.CreatedAt.UTC().Format(rfc3339Nano),
		UpdatedAt:         entity.UpdatedAt.UTC().Format(rfc3339Nano),
	}
	if entity.ReferenceInternal != nil {
		view.ReferenceInternal = entity.ReferenceInternal.String()
	}
	return view
}

// rfc3339Nano is the timestamp layout every API response uses.
const rfc3339Nano = time.RFC3339Nano

func isNotFound(err error) bool { return errors.Is(err, repository.ErrNotFound) }
