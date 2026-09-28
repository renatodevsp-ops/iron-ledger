package domain

import (
	"context"
	"time"
)

// Clock is the only source of "now" in the domain, so a test can pin business
// time deterministically.
type Clock interface {
	Now() time.Time
}

// SystemClock reads the host clock. It is the only implementation that touches
// time.Now.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// IDGenerator produces the UUIDs used for wallets, bets, operations, ledger
// entries and outbox events. It is an interface so tests can pin identifiers.
type IDGenerator interface {
	NewUUID() string
}

// OperationStatus is the terminal state of an applied operation. Only
// COMPLETED rows exist: a failed operation leaves no row at all, so a legitimate
// retry after a fix is never poisoned (data-model.md).
type OperationStatus string

const OperationCompleted OperationStatus = "COMPLETED"

// OperationRecord is the idempotency record. It is written in the same
// transaction as the financial effect and holds the original response so a
// replay returns a byte-identical body.
type OperationRecord struct {
	ID              string
	WalletID        string
	TenantID        string
	IdempotencyKey  string
	RequestHash     []byte
	OperationType   OperationType
	TransactionID   string
	BetID           string
	AmountMinor     int64
	Currency        Currency
	Status          OperationStatus
	ResultCode      string
	ResultBody      []byte
	Channel         Channel
	Actor           string
	CreatedUnixNano int64
}

// AuditOutcome is the accepted/rejected verdict written to audit_log.
type AuditOutcome string

const (
	AuditAccepted AuditOutcome = "ACCEPTED"
	AuditRejected AuditOutcome = "REJECTED"
)

// AuditRecord is one audit line. Every request that reaches this service
// produces exactly one, rejections included, which is what makes "100% das
// recusas são justificadas por um motivo registrado" (SC-010) testable.
type AuditRecord struct {
	OccurredUnixNano int64
	TenantID         string
	WalletID         string
	Actor            string
	Channel          Channel
	OperationType    OperationType
	IdempotencyKey   string
	TransactionID    string
	MessageID        string
	Outcome          AuditOutcome
	ReasonCode       ReasonCode
	Detail           []byte // JSON, non-sensitive context only
}

// OutboxEvent is a row of the transactional outbox. It is inserted in the same
// transaction as the ledger entries and published only after the commit
// succeeds (Constitution Principle V).
type OutboxEvent struct {
	EventUID        string
	AggregateType   string
	AggregateID     string
	EventType       string
	Payload         []byte // JSON, monetary fields are amountMinor + currency
	MessageGroupID  string // wallet_id: the FIFO group
	DedupID         string // event_uid: SQS MessageDeduplicationId
	CreatedUnixNano int64
}

// Event types published on wallet-events.fifo.
const (
	EventBetPlaced   = "wallet.bet_placed"
	EventBetSettled  = "wallet.bet_settled"
	EventBetReversed = "wallet.bet_reversed"
	EventRefunded    = "wallet.refunded"
)

// LedgerRepository is the persistence port for wallets, bets and the
// append-only ledger. Every mutating method must be called with the wallet row
// already locked (Constitution Principle VII).
type LedgerRepository interface {
	// LockWallet takes the per-wallet row lock. It is the first statement of
	// every mutating transaction. Returns ErrWalletNotFound when absent.
	LockWallet(ctx context.Context, walletID string) (*Wallet, error)
	// FindWallet reads a wallet without locking it (balance reads).
	FindWallet(ctx context.Context, walletID string) (*Wallet, error)
	// UpdateBalance applies a signed delta atomically. The CHECK on
	// wallets.balance_minor is the last line of defence.
	UpdateBalance(ctx context.Context, walletID string, delta int64) error
	// NextSequence returns the next per-wallet ledger sequence.
	NextSequence(ctx context.Context, walletID string) (int64, error)
	// AppendEntry writes one immutable ledger line.
	AppendEntry(ctx context.Context, entry LedgerEntry) error
	// CreateBet registers a new bet.
	CreateBet(ctx context.Context, bet Bet) error
	// GetBet reads a bet without locking it.
	GetBet(ctx context.Context, betID string) (*Bet, error)
	// LockBet reads a bet with FOR UPDATE.
	LockBet(ctx context.Context, betID string) (*Bet, error)
	// SetBetStatus persists a legal status transition.
	SetBetStatus(ctx context.Context, bet Bet) error
	// FindBetByExternalRef resolves a caller's external bet reference.
	FindBetByExternalRef(ctx context.Context, tenantID, ref string) (*Bet, error)
	// FindEntriesByOperation lists the entries a previous operation appended.
	FindEntriesByOperation(ctx context.Context, operationID string) ([]LedgerEntry, error)
	// CountEntriesByType counts entries of a type for a wallet, used to prove a
	// debit has not been refunded twice.
	CountEntriesByType(ctx context.Context, walletID, betID string, t EntryType) (int64, error)
}

// OperationsRepository is the persistence port for the idempotency record.
type OperationsRepository interface {
	// ClaimIdempotency inserts the record with ON CONFLICT DO NOTHING. It
	// returns true when this request is the first for the key (the caller then
	// proceeds to apply the effect) and false when the key already exists
	// (research.md D-6).
	ClaimIdempotency(ctx context.Context, rec OperationRecord) (bool, error)
	// GetByIdempotencyKey reads the stored original result for a replay.
	GetByIdempotencyKey(ctx context.Context, walletID, key string) (*OperationRecord, error)
	// FindByTransactionID enforces UNIQUE (wallet_id, transaction_id).
	FindByTransactionID(ctx context.Context, walletID, transactionID string) (*OperationRecord, error)
	// FindByID loads the target of a ROLLBACK.
	FindByID(ctx context.Context, operationID string) (*OperationRecord, error)
}

// AuditWriter is the persistence port for audit_log.
type AuditWriter interface {
	Record(ctx context.Context, rec AuditRecord) error
}

// OutboxWriter is the persistence port for the transactional outbox.
type OutboxWriter interface {
	Enqueue(ctx context.Context, e OutboxEvent) error
}

// Tx bundles the repositories bound to a single database transaction. The
// domain owns the bundle; the platform decides what a transaction is.
type Tx struct {
	Ledger     LedgerRepository
	Operations OperationsRepository
	Audit      AuditWriter
	Outbox     OutboxWriter
}

// UnitOfWork runs a function inside one database transaction, committing on
// success and rolling back on error or panic. Serialization failures and
// deadlocks are retried by the implementation; business rejections are not.
type UnitOfWork interface {
	Do(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}
