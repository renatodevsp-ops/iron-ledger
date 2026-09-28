// Package usecase holds the application services. It depends on the domain
// ports and the standard library only: no HTTP, no pgx, no SQS, no fx. Both
// channels call the same Apply, which is what makes the API and the queue
// equivalent (FR-003).
package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ironledger/ironledger/internal/domain"
)

// Service is the single application service behind both channels. Every
// mutating request, whatever transport it arrived on, goes through Apply.
type Service struct {
	uow   domain.UnitOfWork
	clock domain.Clock
	ids   domain.IDGenerator
}

// New builds the application service. The UnitOfWork is the transaction
// boundary, so no adapter can open an unretried transaction around money.
func New(uow domain.UnitOfWork, clock domain.Clock, ids domain.IDGenerator) *Service {
	return &Service{uow: uow, clock: clock, ids: ids}
}

// ApplyInput is the transport-independent request. The HTTP handler and the SQS
// consumer both build exactly this, so the two channels cannot drift.
type ApplyInput struct {
	WalletID       string
	TenantID       string
	IdempotencyKey string
	Operation      domain.Operation
	Channel        domain.Channel
	Actor          string
	MessageID      string
}

// ApplyOutput carries the response body to emit verbatim. Body is the stored
// result_body, so a replay returns identical bytes.
type ApplyOutput struct {
	Body   json.RawMessage
	Replay bool
}

// OperationResult mirrors contracts/openapi.yaml OperationResult. Every
// monetary field is an int64 in the currency's minimum unit.
type OperationResult struct {
	OperationID         string               `json:"operationId"`
	Type                domain.OperationType `json:"type"`
	Status              string               `json:"status"`
	BetID               *string              `json:"betId"`
	BalanceMinor        int64                `json:"balanceMinor"`
	Currency            domain.Currency      `json:"currency"`
	LedgerEntryIDs      []string             `json:"ledgerEntryIds"`
	ReversedOperationID *string              `json:"reversedOperationId"`
	OccurredAt          time.Time            `json:"occurredAt"`
}

// errRollback aborts the business transaction without surfacing as a failure. A
// rejection must leave no operations row, no ledger entry and no balance change;
// the audit record is then written on its own, so "rejected" and "recorded why
// it was rejected" are both true.
var errRollback = errors.New("business transaction rolled back")

// Apply is the one entry point for all five operation types.
//
// Ordering is deliberate:
//
//  1. lock the wallet row (per-wallet coordination, Constitution VII);
//  2. validate, allocate the sequence and compute the complete result;
//  3. claim the idempotency key, storing the final body;
//  4. write the ledger entry, the balance and the outbox row;
//  5. write the audit row in the same transaction.
//
// Locking before the claim avoids the KEY SHARE to FOR UPDATE upgrade that two
// concurrent same-key transactions would otherwise deadlock on, and it makes
// "a duplicate key is always visible" true: for one (wallet, key) pair only one
// transaction can be past the lock at a time.
//
// Because the result body is known at step 3, the idempotency record is inserted
// once and never updated. That is why the application role has no UPDATE on the
// operations table.
func (s *Service) Apply(ctx context.Context, in ApplyInput) (ApplyOutput, error) {
	// Rejections that happen before the business transaction still have to be
	// recorded: SC-010 requires every refusal to have a reason on file, not only
	// the ones that reached the database.
	if err := ValidateIdempotencyKey(in.IdempotencyKey); err != nil {
		return ApplyOutput{}, s.reject(ctx, in, asDomainError(err))
	}
	if err := in.Operation.Validate(); err != nil {
		return ApplyOutput{}, s.reject(ctx, in, asDomainError(err))
	}

	hash := RequestHash(in.Operation)
	var (
		out    ApplyOutput
		reject *domain.Error
	)
	err := s.uow.Do(ctx, func(ctx context.Context, tx domain.Tx) error {
		res, appErr := s.applyInTx(ctx, tx, in, hash)
		if appErr != nil {
			reject = appErr
			return errRollback
		}
		if err := tx.Audit.Record(ctx, s.acceptedAudit(in, res)); err != nil {
			return err
		}
		out = res
		return nil
	})

	if reject != nil {
		// The business transaction rolled back, so nothing was written. The
		// rejection still has to be recorded.
		return ApplyOutput{}, s.reject(ctx, in, reject)
	}
	if err != nil {
		return ApplyOutput{}, err
	}
	return out, nil
}

// reject records the reason and returns it. If the audit write fails the caller
// is told the dependency is unavailable instead: a rejection nobody recorded
// would silently break SC-010, and the operation is safe to retry either way.
func (s *Service) reject(ctx context.Context, in ApplyInput, reason *domain.Error) error {
	if err := s.recordRejection(ctx, in, reason); err != nil {
		return err
	}
	return reason
}

// applyInTx is the body of the one transaction. It never commits on a
// rejection: it returns the typed reason and the caller rolls back.
func (s *Service) applyInTx(ctx context.Context, tx domain.Tx, in ApplyInput, hash []byte) (ApplyOutput, *domain.Error) {
	// 1. The wallet row lock is the first statement of the transaction.
	wallet, err := tx.Ledger.LockWallet(ctx, in.WalletID)
	if err != nil {
		return ApplyOutput{}, asDomainError(err)
	}
	if wallet.TenantID != in.TenantID {
		return ApplyOutput{}, domain.ErrTenantMismatch
	}

	// 2. A distinct operation reusing a transactionId is refused before the key
	//    is claimed, so a caller mistake does not burn an idempotency key. The
	//    UNIQUE (wallet_id, transaction_id) constraint stays the authority.
	existing, err := tx.Operations.FindByTransactionID(ctx, wallet.ID, in.Operation.TransactionID)
	if err != nil {
		return ApplyOutput{}, asDomainError(err)
	}
	if existing != nil && existing.IdempotencyKey != in.IdempotencyKey {
		return ApplyOutput{}, domain.ErrDuplicateTransactionID
	}

	// 3. Dispatch by operation type.
	switch in.Operation.Type {
	case domain.OperationBET:
		return s.applyBet(ctx, tx, wallet, in, hash)
	default:
		return ApplyOutput{}, domain.NewError(domain.ReasonInvalidOperationState,
			"%s is not available in this build", in.Operation.Type)
	}
}

// applyBet is the MVP branch: place a bet and debit its stake.
func (s *Service) applyBet(
	ctx context.Context,
	tx domain.Tx,
	wallet *domain.Wallet,
	in ApplyInput,
	hash []byte,
) (ApplyOutput, *domain.Error) {
	now := s.clock.Now()
	operationID := s.ids.NewUUID()
	amount := in.Operation.Amount

	// Wallet.Debit enforces the three rules the database also enforces: right
	// currency, active wallet, and a balance that would not go negative.
	updated, err := wallet.Debit(amount)
	if err != nil {
		return ApplyOutput{}, asDomainError(err)
	}
	balanceAfter := updated.Balance.AmountMinor

	sequence, err := tx.Ledger.NextSequence(ctx, wallet.ID)
	if err != nil {
		return ApplyOutput{}, asDomainError(err)
	}

	betID := in.Operation.BetID
	if betID == "" {
		betID = s.ids.NewUUID()
	}

	entry, err := domain.NewEntry(domain.LedgerEntry{
		EntryID:            s.ids.NewUUID(),
		WalletID:           wallet.ID,
		TenantID:           wallet.TenantID,
		OperationID:        operationID,
		BetID:              betID,
		EntryType:          domain.EntryBetDebit,
		Direction:          domain.DirectionDebit,
		AmountMinor:        amount.AmountMinor,
		Currency:           amount.Currency,
		BalanceAfterMinor:  balanceAfter,
		Sequence:           sequence,
		OccurredAtUnixNano: now.UnixNano(),
	})
	if err != nil {
		return ApplyOutput{}, asDomainError(err)
	}

	result := OperationResult{
		OperationID:    operationID,
		Type:           domain.OperationBET,
		Status:         string(domain.OperationCompleted),
		BetID:          &betID,
		BalanceMinor:   balanceAfter,
		Currency:       amount.Currency,
		LedgerEntryIDs: []string{entry.EntryID},
		OccurredAt:     now.UTC(),
	}
	body, err := json.Marshal(result)
	if err != nil {
		return ApplyOutput{}, asDomainError(domain.NewError(domain.ReasonInternalError,
			"encode result: %s", err))
	}

	// 4. Claim the key, storing the final body.
	record := domain.OperationRecord{
		ID:              operationID,
		WalletID:        wallet.ID,
		TenantID:        wallet.TenantID,
		IdempotencyKey:  in.IdempotencyKey,
		RequestHash:     hash,
		OperationType:   in.Operation.Type,
		TransactionID:   in.Operation.TransactionID,
		BetID:           betID,
		AmountMinor:     amount.AmountMinor,
		Currency:        amount.Currency,
		Status:          domain.OperationCompleted,
		ResultCode:      "COMPLETED",
		ResultBody:      body,
		Channel:         in.Channel,
		Actor:           in.Actor,
		CreatedUnixNano: now.UnixNano(),
	}
	claimed, err := tx.Operations.ClaimIdempotency(ctx, record)
	if err != nil {
		return ApplyOutput{}, asDomainError(err)
	}
	if !claimed {
		// Already applied: return the stored original, never our recomputation.
		// A different body under the same key is a conflict, not a replay.
		replay, replayErr := loadStoredResult(ctx, tx, wallet.ID, in.IdempotencyKey, hash)
		if replayErr != nil {
			return ApplyOutput{}, asDomainError(replayErr)
		}
		return replay, nil
	}

	// 5. Persist the effect. external_bet_ref is NOT NULL and unique per
	//    tenant; when the caller supplies no reference the bet's own id is used,
	//    so the constraint holds and the caller still finds the bet via betId.
	externalRef := in.Operation.ReferenceID
	if externalRef == "" {
		externalRef = "bet:" + betID
	}
	if err := tx.Ledger.CreateBet(ctx, domain.Bet{
		ID:             betID,
		WalletID:       wallet.ID,
		TenantID:       wallet.TenantID,
		ExternalBetRef: externalRef,
		Stake:          amount,
		Status:         domain.BetOpen,
		CreatedAt:      now.UnixNano(),
	}); err != nil {
		return ApplyOutput{}, asDomainError(err)
	}
	if err := tx.Ledger.AppendEntry(ctx, entry); err != nil {
		return ApplyOutput{}, asDomainError(err)
	}
	// The signed delta is applied atomically; CHECK (balance_minor >= 0) is the
	// database's guarantee if this ever went wrong.
	if err := tx.Ledger.UpdateBalance(ctx, wallet.ID, -amount.AmountMinor); err != nil {
		return ApplyOutput{}, asDomainError(err)
	}

	// The outbox row shares this transaction, so a rollback leaves no event and
	// a commit can never lose one (Constitution Principle V). The SQS group is
	// the wallet and the deduplication id is the event uid.
	eventUID := s.ids.NewUUID()
	payload, err := json.Marshal(map[string]any{
		"eventUid":      eventUID,
		"walletId":      wallet.ID,
		"betId":         betID,
		"operationId":   operationID,
		"type":          string(domain.OperationBET),
		"amountMinor":   amount.AmountMinor,
		"currency":      string(amount.Currency),
		"balanceMinor":  balanceAfter,
		"transactionId": in.Operation.TransactionID,
		"occurredAt":    now.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return ApplyOutput{}, asDomainError(domain.NewError(domain.ReasonInternalError,
			"encode event payload: %s", err))
	}
	if err := tx.Outbox.Enqueue(ctx, domain.OutboxEvent{
		EventUID:        eventUID,
		AggregateType:   "bet",
		AggregateID:     betID,
		EventType:       domain.EventBetPlaced,
		Payload:         payload,
		MessageGroupID:  wallet.ID,
		DedupID:         eventUID,
		CreatedUnixNano: now.UnixNano(),
	}); err != nil {
		return ApplyOutput{}, asDomainError(err)
	}

	// 6. Read the stored body back so the first response and every replay are the
	//    same bytes, whatever the database's JSON normalization does to it.
	final, err := loadStoredResult(ctx, tx, wallet.ID, in.IdempotencyKey, hash)
	if err != nil {
		return ApplyOutput{}, asDomainError(err)
	}
	return ApplyOutput{Body: final.Body, Replay: false}, nil
}

// acceptedAudit builds the ACCEPTED audit record for an applied operation.
func (s *Service) acceptedAudit(in ApplyInput, out ApplyOutput) domain.AuditRecord {
	detail, _ := json.Marshal(map[string]any{"replay": out.Replay})
	return domain.AuditRecord{
		OccurredUnixNano: s.clock.Now().UnixNano(),
		TenantID:         in.TenantID,
		WalletID:         in.WalletID,
		Actor:            in.Actor,
		Channel:          in.Channel,
		OperationType:    in.Operation.Type,
		IdempotencyKey:   in.IdempotencyKey,
		TransactionID:    in.Operation.TransactionID,
		MessageID:        in.MessageID,
		Outcome:          domain.AuditAccepted,
		Detail:           detail,
	}
}

// recordRejection writes the audit row for a rejected request in its own
// transaction, after the business transaction has rolled back. A rejected
// request therefore leaves no operations row, so a legitimate retry after a fix
// is never blocked.
func (s *Service) recordRejection(ctx context.Context, in ApplyInput, reason *domain.Error) error {
	detail, _ := json.Marshal(map[string]any{
		"code":    string(reason.Code),
		"message": reason.Message,
		"field":   reason.Field,
	})
	rec := domain.AuditRecord{
		OccurredUnixNano: s.clock.Now().UnixNano(),
		TenantID:         in.TenantID,
		WalletID:         in.WalletID,
		Actor:            in.Actor,
		Channel:          in.Channel,
		OperationType:    in.Operation.Type,
		IdempotencyKey:   in.IdempotencyKey,
		TransactionID:    in.Operation.TransactionID,
		MessageID:        in.MessageID,
		Outcome:          domain.AuditRejected,
		ReasonCode:       reason.Code,
		Detail:           detail,
	}
	if err := s.uow.Do(ctx, func(ctx context.Context, tx domain.Tx) error {
		return tx.Audit.Record(ctx, rec)
	}); err != nil {
		return domain.NewError(domain.ReasonDependencyUnavailable,
			"request rejected (%s) but the rejection could not be recorded: %s", reason.Code, err)
	}
	return nil
}

// asDomainError normalizes any error into a typed domain error so the transport
// always has a stable reason code to publish.
func asDomainError(err error) *domain.Error {
	if err == nil {
		return nil
	}
	var domErr *domain.Error
	if errors.As(err, &domErr) {
		return domErr
	}
	return domain.NewError(domain.ReasonInternalError, "%s", err)
}
