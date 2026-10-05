// Package domain is the WagerTransaction aggregate: one external operation
// reported by a game provider, or the internal opening of a wallet.
//
// The aggregate owns the state machine. A provider can do whatever it likes
// with the wire format — send a LOSS with a value, name a reference that has
// not arrived yet, try to reverse the same bet twice — and the transitions
// below refuse it. Terminal states are terminal: a replayed request reads the
// persisted outcome instead of moving money again.
package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// Kind is the operation a provider is reporting.
type Kind string

const (
	// KindOpening is the internal opening of a wallet. It is never accepted
	// from a provider, over HTTP or over SQS.
	KindOpening Kind = "OPENING"
	// KindBet debits the wallet.
	KindBet Kind = "BET"
	// KindWin credits the wallet.
	KindWin Kind = "WIN"
	// KindLoss records a lost bet: a business fact with no financial effect.
	KindLoss Kind = "LOSS"
	// KindRefund credits back a processed bet.
	KindRefund Kind = "REFUND"
	// KindRollback reverses a processed bet, win or refund.
	KindRollback Kind = "ROLLBACK"
)

// ExternalKinds are the kinds a provider may report.
var ExternalKinds = []Kind{KindBet, KindWin, KindLoss, KindRefund, KindRollback}

// AllKinds includes the internal opening.
var AllKinds = append([]Kind{KindOpening}, ExternalKinds...)

// Status is the lifecycle position of a transaction.
type Status string

const (
	// StatusPending means the record was accepted and processing has not
	// finished. Every confirmed PENDING is resumable by another instance.
	StatusPending Status = "PENDING"
	// StatusPendingReference means processing is waiting for a reference that
	// has not arrived yet, or has not been processed.
	StatusPendingReference Status = "PENDING_REFERENCE"
	// StatusProcessed means the operation completed successfully. Terminal.
	StatusProcessed Status = "PROCESSED"
	// StatusRejected means a business rule refused the operation. Terminal.
	StatusRejected Status = "REJECTED"
	// StatusFailed means an infrastructure failure was recorded for audit.
	// Terminal.
	StatusFailed Status = "FAILED"
)

// Origin tells apart the platform's own operations from a provider's.
type Origin string

const (
	// OriginInternal is an operation this platform started.
	OriginInternal Origin = "INTERNAL"
	// OriginExternal is an operation reported by a provider.
	OriginExternal Origin = "EXTERNAL"
)

// Statuses lists every status, in lifecycle order.
func Statuses() []Status {
	return []Status{StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed}
}

// Terminal reports whether no further transition is allowed from s.
func (s Status) Terminal() bool {
	return s == StatusProcessed || s == StatusRejected || s == StatusFailed
}

// ParseKind validates an operation kind reported by a provider.
//
// KindOpening is refused here, at the boundary: only the platform may open a
// wallet, and a provider that claims to is not authenticated as one.
func ParseKind(raw string) (Kind, error) {
	kind := Kind(raw)
	for _, candidate := range ExternalKinds {
		if kind == candidate {
			return kind, nil
		}
	}
	if kind == KindOpening {
		return "", xerr.Validation(xerr.CodeKindNotAccepted, "Operation kind not accepted",
			"$kind is reserved for the internal opening of a wallet and cannot be reported by a provider.",
			map[string]any{"kind": string(kind)})
	}
	return "", xerr.Validation(xerr.CodeKindNotAccepted, "Unknown operation kind",
		"$kind is not one of the accepted operation kinds.",
		map[string]any{"kind": raw, "accepted": "BET, WIN, LOSS, REFUND, ROLLBACK"})
}

// ParseKindInternal validates a kind on the internal path, where the opening
// is allowed.
func ParseKindInternal(raw string) (Kind, error) {
	kind := Kind(raw)
	for _, candidate := range AllKinds {
		if kind == candidate {
			return kind, nil
		}
	}
	return "", xerr.Validation(xerr.CodeKindNotAccepted, "Unknown operation kind",
		"$kind is not one of the accepted operation kinds.", map[string]any{"kind": raw})
}

// ValidateAmount enforces the zero-amount policy of each kind.
//
// BET, WIN, REFUND and ROLLBACK must carry value: a zero-amount movement is
// not a movement and would produce no ledger entry. LOSS must carry exactly
// zero: it is the record of a bet that moved nothing, so a value on it would
// mean money changed hands without a ledger entry to prove it.
func (k Kind) ValidateAmount(amount money.Money) error {
	if k == KindLoss {
		if !amount.IsZero() {
			return xerr.Validation(xerr.CodeLossAmountMustBeZero, "Loss must carry no amount",
				"A LOSS records a bet that moved no money, so its amount must be exactly 0.00. It received $amount.",
				map[string]any{"amount": amount.String()})
		}
		return nil
	}
	if !amount.IsPositive() {
		return xerr.Validation(xerr.CodeAmountMustBePositive, "Amount must be positive",
			"A $kind operation moves value, so its amount must be greater than 0.00.",
			map[string]any{"kind": string(k), "amount": amount.String()})
	}
	return nil
}

// RequiresReference reports whether the kind must name the operation it acts on.
func (k Kind) RequiresReference() bool { return k == KindRefund || k == KindRollback }

// MovesMoney reports whether the kind changes a balance.
func (k Kind) MovesMoney() bool {
	return k == KindOpening || k == KindBet || k == KindWin || k == KindRefund || k == KindRollback
}

// Direction returns the wallet direction the kind implies.
func (k Kind) Direction() (string, bool) {
	switch k {
	case KindOpening, KindWin, KindRefund:
		return "CREDIT", true
	case KindBet:
		return "DEBIT", true
	default:
		return "", false
	}
}

// ReversibleKinds are the kinds a ROLLBACK may act on.
var ReversibleKinds = []Kind{KindBet, KindWin, KindRefund}

// IsReversible reports whether a ROLLBACK may reference a transaction of kind
// k. A refund is the reversal of a bet; rolling back a refund gives the money
// back to where it came from.
func IsReversible(kind Kind) bool {
	for _, candidate := range ReversibleKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// RefundableKinds are the kinds a REFUND may act on.
var RefundableKinds = []Kind{KindBet}

// IsRefundable reports whether a REFUND may reference a transaction of kind k.
func IsRefundable(kind Kind) bool {
	for _, candidate := range RefundableKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// Registration is the data a transaction is created with.
type Registration struct {
	ID                  uuid.UUID
	Origin              Origin
	ProviderID          string
	ExternalID          string
	IdempotencyKey      string
	PayloadHash         string
	WalletID            uuid.UUID
	PlayerID            uuid.UUID
	RoundID             string
	GameID              string
	Kind                Kind
	Amount              money.Money
	ReferenceExternalID *string
	ReferenceInternalID *uuid.UUID
	ReferenceResolvedAt *time.Time
	FailureCode         xerr.Code
	FailureMessage      string
	RecordedAt          time.Time
	PendingAttempts     int
	NextAttemptAt       *time.Time
}

// WagerTransaction is the aggregate root guarding one operation's lifecycle.
type WagerTransaction struct {
	id                  uuid.UUID
	origin              Origin
	providerID          string
	externalID          string
	idempotencyKey      string
	payloadHash         string
	walletID            uuid.UUID
	playerID            uuid.UUID
	roundID             string
	gameID              string
	kind                Kind
	amount              money.Money
	referenceExternalID *string
	referenceInternalID *uuid.UUID
	referenceResolvedAt *time.Time
	status              Status
	failureCode         xerr.Code
	failureMessage      string
	balanceAfter        *money.Money
	pendingAttempts     int
	nextAttemptAt       *time.Time
	createdAt           time.Time
	updatedAt           time.Time
}

// Snapshot is the plain projection of a WagerTransaction.
type Snapshot struct {
	ID                  uuid.UUID
	Origin              Origin
	ProviderID          string
	ExternalID          string
	IdempotencyKey      string
	PayloadHash         string
	WalletID            uuid.UUID
	PlayerID            uuid.UUID
	RoundID             string
	GameID              string
	Kind                Kind
	Amount              money.Money
	ReferenceExternalID *string
	ReferenceInternalID *uuid.UUID
	ReferenceResolvedAt *time.Time
	Status              Status
	FailureCode         xerr.Code
	FailureMessage      string
	BalanceAfter        *money.Money
	PendingAttempts     int
	NextAttemptAt       *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Register accepts a new operation in PENDING.
//
// It performs no movement: acceptance and execution are separate, and only
// settlement moves money. The one exception is the internal opening, which is
// created already settled, because opening a wallet and crediting it are one
// indivisible act from the platform's point of view.
func Register(r Registration, at time.Time) (WagerTransaction, error) {
	if r.ID == uuid.Nil {
		return WagerTransaction{}, xerr.Validation(xerr.CodeInvalidRequest, "Missing transaction id",
			"$field is required.", map[string]any{"field": "transactionId"})
	}
	if r.WalletID == uuid.Nil {
		return WagerTransaction{}, xerr.Validation(xerr.CodeInvalidRequest, "Missing wallet",
			"$field is required.", map[string]any{"field": "walletId"})
	}
	if r.PlayerID == uuid.Nil {
		return WagerTransaction{}, xerr.Validation(xerr.CodeInvalidRequest, "Missing player",
			"$field is required.", map[string]any{"field": "playerId"})
	}
	if r.Amount.Currency() == "" {
		return WagerTransaction{}, xerr.Validation(xerr.CodeInvalidRequest, "Missing currency",
			"$field is required.", map[string]any{"field": "money.currency"})
	}
	if r.Origin == OriginExternal {
		if r.ProviderID == "" {
			return WagerTransaction{}, xerr.Validation(xerr.CodeInvalidRequest, "Missing provider",
				"$field is required for an operation reported by a provider.", map[string]any{"field": "providerId"})
		}
		if r.ExternalID == "" {
			return WagerTransaction{}, xerr.Validation(xerr.CodeInvalidRequest, "Missing external transaction",
				"$field is required for an operation reported by a provider.",
				map[string]any{"field": "externalTransactionId"})
		}
		if r.IdempotencyKey == "" {
			return WagerTransaction{}, xerr.Validation(xerr.CodeInvalidRequest, "Missing idempotency key",
				"$field is required so a repeated delivery can be recognised.",
				map[string]any{"field": "idempotencyKey"})
		}
		if err := r.Kind.ValidateAmount(r.Amount); err != nil {
			return WagerTransaction{}, err
		}
		if r.Kind.RequiresReference() && (r.ReferenceExternalID == nil || *r.ReferenceExternalID == "") {
			return WagerTransaction{}, xerr.Validation(xerr.CodeReferenceRequired, "Reference required",
				"A $kind must name the operation it acts on through $field.",
				map[string]any{"kind": string(r.Kind), "field": "referenceExternalTransactionId"})
		}
	}
	at = at.UTC()
	tx := WagerTransaction{
		id:                  r.ID,
		origin:              r.Origin,
		providerID:          r.ProviderID,
		externalID:          r.ExternalID,
		idempotencyKey:      r.IdempotencyKey,
		payloadHash:         r.PayloadHash,
		walletID:            r.WalletID,
		playerID:            r.PlayerID,
		roundID:             r.RoundID,
		gameID:              r.GameID,
		kind:                r.Kind,
		amount:              r.Amount,
		referenceExternalID: r.ReferenceExternalID,
		createdAt:           at,
		updatedAt:           at,
		status:              StatusPending,
	}
	if r.NextAttemptAt != nil {
		tx.nextAttemptAt = r.NextAttemptAt
	}
	return tx, nil
}

// Rehydrate rebuilds a transaction from persisted state.
//
// It validates but performs no transition and emits no event: rebuilding state
// is not an event.
func Rehydrate(s Snapshot) (WagerTransaction, error) {
	if s.ID == uuid.Nil || s.WalletID == uuid.Nil {
		return WagerTransaction{}, xerr.Infrastructure("persisted transaction is missing its identity", nil)
	}
	known := false
	for _, status := range Statuses() {
		if s.Status == status {
			known = true
			break
		}
	}
	if !known {
		return WagerTransaction{}, xerr.Infrastructure("persisted transaction carries an unknown status", nil)
	}
	return WagerTransaction{
		id:                  s.ID,
		origin:              s.Origin,
		providerID:          s.ProviderID,
		externalID:          s.ExternalID,
		idempotencyKey:      s.IdempotencyKey,
		payloadHash:         s.PayloadHash,
		walletID:            s.WalletID,
		playerID:            s.PlayerID,
		roundID:             s.RoundID,
		gameID:              s.GameID,
		kind:                s.Kind,
		amount:              s.Amount,
		referenceExternalID: s.ReferenceExternalID,
		referenceInternalID: s.ReferenceInternalID,
		referenceResolvedAt: s.ReferenceResolvedAt,
		status:              s.Status,
		failureCode:         s.FailureCode,
		failureMessage:      s.FailureMessage,
		balanceAfter:        s.BalanceAfter,
		pendingAttempts:     s.PendingAttempts,
		nextAttemptAt:       s.NextAttemptAt,
		createdAt:           s.CreatedAt,
		updatedAt:           s.UpdatedAt,
	}, nil
}

// Accessors.

// ID returns the internal transaction identity.
func (t WagerTransaction) ID() uuid.UUID { return t.id }

// Origin returns whether the operation came from a provider or from us.
func (t WagerTransaction) Origin() Origin { return t.origin }

// ProviderID returns the reporting provider, empty for internal operations.
func (t WagerTransaction) ProviderID() string { return t.providerID }

// ExternalID returns the provider's own identity of the operation.
func (t WagerTransaction) ExternalID() string { return t.externalID }

// IdempotencyKey returns the key the operation is deduplicated by.
func (t WagerTransaction) IdempotencyKey() string { return t.idempotencyKey }

// PayloadHash returns the canonical hash of the business payload.
func (t WagerTransaction) PayloadHash() string { return t.payloadHash }

// WalletID returns the wallet the operation acts on.
func (t WagerTransaction) WalletID() uuid.UUID { return t.walletID }

// PlayerID returns the player the wallet belongs to.
func (t WagerTransaction) PlayerID() uuid.UUID { return t.playerID }

// RoundID returns the game round the operation belongs to.
func (t WagerTransaction) RoundID() string { return t.roundID }

// GameID returns the game the round belongs to.
func (t WagerTransaction) GameID() string { return t.gameID }

// Kind returns the operation kind.
func (t WagerTransaction) Kind() Kind { return t.kind }

// Amount returns the operation amount.
func (t WagerTransaction) Amount() money.Money { return t.amount }

// ReferenceExternalID returns the external reference of a reversal, if any.
func (t WagerTransaction) ReferenceExternalID() *string { return t.referenceExternalID }

// ReferenceInternalID returns the resolved internal reference, if any.
func (t WagerTransaction) ReferenceInternalID() *uuid.UUID { return t.referenceInternalID }

// ReferenceResolvedAt returns when the reference was resolved, if it was.
func (t WagerTransaction) ReferenceResolvedAt() *time.Time { return t.referenceResolvedAt }

// Status returns the lifecycle position.
func (t WagerTransaction) Status() Status { return t.status }

// FailureCode returns the stable code of a rejection or failure.
func (t WagerTransaction) FailureCode() xerr.Code { return t.failureCode }

// FailureMessage returns the human readable reason of a rejection or failure.
func (t WagerTransaction) FailureMessage() string { return t.failureMessage }

// BalanceAfter returns the balance observed when the operation was processed,
// which is what a replay reports even after the wallet has moved on.
func (t WagerTransaction) BalanceAfter() *money.Money { return t.balanceAfter }

// PendingAttempts returns how many times a pending reference has been retried.
func (t WagerTransaction) PendingAttempts() int { return t.pendingAttempts }

// NextAttemptAt returns when the pending reference should be retried.
func (t WagerTransaction) NextAttemptAt() *time.Time { return t.nextAttemptAt }

// CreatedAt returns the acceptance instant.
func (t WagerTransaction) CreatedAt() time.Time { return t.createdAt }

// UpdatedAt returns the instant of the last transition.
func (t WagerTransaction) UpdatedAt() time.Time { return t.updatedAt }

// Snapshot projects the transaction into plain data.
func (t WagerTransaction) Snapshot() Snapshot {
	return Snapshot{
		ID:                  t.id,
		Origin:              t.origin,
		ProviderID:          t.providerID,
		ExternalID:          t.externalID,
		IdempotencyKey:      t.idempotencyKey,
		PayloadHash:         t.payloadHash,
		WalletID:            t.walletID,
		PlayerID:            t.playerID,
		RoundID:             t.roundID,
		GameID:              t.gameID,
		Kind:                t.kind,
		Amount:              t.amount,
		ReferenceExternalID: t.referenceExternalID,
		ReferenceInternalID: t.referenceInternalID,
		ReferenceResolvedAt: t.referenceResolvedAt,
		Status:              t.status,
		FailureCode:         t.failureCode,
		FailureMessage:      t.failureMessage,
		BalanceAfter:        t.balanceAfter,
		PendingAttempts:     t.pendingAttempts,
		NextAttemptAt:       t.nextAttemptAt,
		CreatedAt:           t.createdAt,
		UpdatedAt:           t.updatedAt,
	}
}

// Transitions.

// Settle moves the transaction to PROCESSED.
//
// It is the only transition that reports a financial result, and the balance it
// carries is the one observed at processing time. From PENDING_REFERENCE it
// resolves the reference that was awaited.
func (t WagerTransaction) Settle(referenceInternalID *uuid.UUID, balanceAfter money.Money, at time.Time) (WagerTransaction, error) {
	if err := t.assertNotTerminal("settle"); err != nil {
		return WagerTransaction{}, err
	}
	if balanceAfter.Currency() != t.amount.Currency() {
		return WagerTransaction{}, xerr.Validation(xerr.CodeCurrencyMismatch, "Currency mismatch",
			"The resulting balance is in $balance while the operation is in $amount.",
			map[string]any{"balance": string(balanceAfter.Currency()), "amount": string(t.amount.Currency())})
	}
	resolvedAt := at.UTC()
	t.referenceInternalID = referenceInternalID
	if referenceInternalID != nil {
		t.referenceResolvedAt = &resolvedAt
	}
	t.balanceAfter = &balanceAfter
	t.status = StatusProcessed
	t.failureCode = ""
	t.failureMessage = ""
	t.nextAttemptAt = nil
	t.updatedAt = resolvedAt
	return t, nil
}

// AwaitReference moves the transaction to PENDING_REFERENCE, or reschedules it
// if it is already waiting.
func (t WagerTransaction) AwaitReference(nextAttemptAt time.Time, reason string, at time.Time) (WagerTransaction, error) {
	if err := t.assertNotTerminal("await reference"); err != nil {
		return WagerTransaction{}, err
	}
	if t.referenceExternalID == nil || *t.referenceExternalID == "" {
		return WagerTransaction{}, xerr.Validation(xerr.CodeReferenceRequired, "Reference required",
			"Only an operation that names a reference can wait for one.",
			map[string]any{"transactionId": t.id.String()})
	}
	t.status = StatusPendingReference
	t.pendingAttempts++
	scheduled := nextAttemptAt.UTC()
	t.nextAttemptAt = &scheduled
	t.failureMessage = reason
	t.updatedAt = at.UTC()
	return t, nil
}

// Reject moves the transaction to REJECTED, a terminal business decision.
func (t WagerTransaction) Reject(code xerr.Code, message string, at time.Time) (WagerTransaction, error) {
	if err := t.assertNotTerminal("reject"); err != nil {
		return WagerTransaction{}, err
	}
	t.status = StatusRejected
	t.failureCode = code
	t.failureMessage = message
	t.nextAttemptAt = nil
	t.updatedAt = at.UTC()
	return t, nil
}

// Fail moves the transaction to FAILED, recording a permanent infrastructure
// failure for audit.
func (t WagerTransaction) Fail(code xerr.Code, message string, at time.Time) (WagerTransaction, error) {
	if err := t.assertNotTerminal("fail"); err != nil {
		return WagerTransaction{}, err
	}
	t.status = StatusFailed
	t.failureCode = code
	t.failureMessage = message
	t.nextAttemptAt = nil
	t.updatedAt = at.UTC()
	return t, nil
}

func (t WagerTransaction) assertNotTerminal(what string) error {
	if t.status.Terminal() {
		return xerr.Conflict(xerr.CodeExternalTransactionConflict, "Transaction already settled",
			"Transaction $transaction is already $status and cannot be $what again.",
			map[string]any{"transaction": t.id.String(), "status": string(t.status), "attempted": what})
	}
	return nil
}

// String renders the aggregate for logs.
func (t WagerTransaction) String() string {
	return fmt.Sprintf("wager transaction %s kind=%s status=%s wallet=%s",
		t.id, t.kind, t.status, t.walletID)
}
