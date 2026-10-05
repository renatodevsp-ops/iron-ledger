// Package usecase is the application layer: the use cases that compose the
// slices of every bounded context into the operations the API and the consumer
// expose.
//
// It is the only layer that knows both HTTP and SQS, and it treats them as the
// same operation. Validation, idempotency, reference resolution and the wallet
// movement live here once, so an operation sent over HTTP and the same
// operation sent over the broker cannot produce different outcomes, different
// hashes or different guarantees.
package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/app/pipeline"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/awaitwagerreference"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/registerwageroperation"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/settlewageroperation"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/wagertransactiondetails"
	wdomain "github.com/ironledger/iron-ledger/internal/domain/wallet/domain"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/applywalletmovement"
	"github.com/ironledger/iron-ledger/internal/messaging/inbox"
	"github.com/ironledger/iron-ledger/internal/platform/config"
	"github.com/ironledger/iron-ledger/internal/platform/logging"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
	"github.com/ironledger/iron-ledger/internal/platform/repository"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/idempotency"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// Source names the transport an operation arrived through. It is recorded for
// metrics and logs only: it never takes part in deduplication, so the same
// operation sent over HTTP and over SQS is one operation.
type Source string

const (
	SourceHTTP   Source = "http"
	SourceSQS    Source = "sqs"
	SourceResume Source = "resume"
)

// OperationRequest is the transport-independent description of a wager
// operation.
//
// HTTP and SQS build this same value from their own payloads, which is what
// makes their guarantees identical rather than merely similar: the validation
// below, and the hash derived from it, run exactly once.
type OperationRequest struct {
	// IdempotencyKey is the key the operation is deduplicated by.
	IdempotencyKey string
	// ProviderID is the provider the operation belongs to. On the HTTP path it
	// comes from the authenticated caller, never from the body.
	ProviderID            string
	ExternalTransactionID string
	PlayerID              uuid.UUID
	WalletID              uuid.UUID
	RoundID               string
	GameID                string
	Kind                  string
	// Amount is the decimal string exactly as received, so the parser — not a
	// caller — decides what it means.
	Amount   string
	Currency string
	// ReferenceExternalID names the operation a reversal acts on.
	ReferenceExternalID *string
	Source              Source
	// Inbox carries the broker identity when the operation arrived over SQS.
	Inbox *InboxContext
}

// InboxContext ties an operation to the message that carried it.
type InboxContext struct {
	ConsumerName string
	MessageID    string
	PayloadHash  string
	ReceivedAt   time.Time
}

// MoneyView is the wire form of a monetary value.
type MoneyView struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// OperationResult is what the caller is told, and what a replay repeats.
type OperationResult struct {
	TransactionID     uuid.UUID  `json:"transactionId"`
	Status            string     `json:"status"`
	Balance           *MoneyView `json:"balance,omitempty"`
	FailureCode       xerr.Code  `json:"failureCode,omitempty"`
	FailureMessage    string     `json:"failureMessage,omitempty"`
	IdempotentReplay  bool       `json:"idempotentReplay"`
	ReferenceExternal string     `json:"referenceExternalTransactionId,omitempty"`
}

func moneyView(m *money.Money) *MoneyView {
	if m == nil {
		return nil
	}
	return &MoneyView{Amount: m.String(), Currency: string(m.Currency())}
}

// WageringUseCase processes provider operations.
//
// Every operation is one PostgreSQL transaction: the idempotency record, the
// events, the wallet balance, the ledger, the transaction index, the inbox
// entry and the outbound events are all committed together or not at all.
type WageringUseCase struct {
	tx      *uow.Manager
	pipe    *pipeline.Pipeline
	cfg     config.Wagering
	logger  *slog.Logger
	metrics *metrics.Metrics

	registerWager     cqrs.CommandHandler[registerwageroperation.Command]
	awaitReferenceCmd cqrs.CommandHandler[awaitwagerreference.Command]
	settleWager       cqrs.CommandHandler[settlewageroperation.Command]
	moveWallet        cqrs.CommandHandler[applywalletmovement.Command]

	wallets      WalletReader
	transactions TransactionReader
	inboxRepo    inbox.Repository
}

// NewWageringUseCase wires the use case to the slices it composes.
func NewWageringUseCase(
	tx *uow.Manager,
	pipe *pipeline.Pipeline,
	cfg config.Wagering,
	logger *slog.Logger,
	m *metrics.Metrics,
	registerWager cqrs.CommandHandler[registerwageroperation.Command],
	awaitReferenceCmd cqrs.CommandHandler[awaitwagerreference.Command],
	settleWager cqrs.CommandHandler[settlewageroperation.Command],
	moveWallet cqrs.CommandHandler[applywalletmovement.Command],
	wallets WalletReader,
	transactions TransactionReader,
	inboxRepo inbox.Repository,
) *WageringUseCase {
	return &WageringUseCase{
		tx:                tx,
		pipe:              pipe,
		cfg:               cfg,
		logger:            logger,
		metrics:           m,
		registerWager:     registerWager,
		awaitReferenceCmd: awaitReferenceCmd,
		settleWager:       settleWager,
		moveWallet:        moveWallet,
		wallets:           wallets,
		transactions:      transactions,
		inboxRepo:         inboxRepo,
	}
}

// Submit processes one operation reported by a provider.
//
// The contract, in order:
//
//   - The idempotency key is mandatory. A key already seen with the same
//     business payload replays the stored outcome, including the balance
//     observed the first time. The same key with a different payload is a
//     conflict. The same (provider, external transaction) under a different key
//     is also a conflict: an operation identified by the provider cannot be
//     applied twice under a second name.
//   - The wallet is resolved and checked before anything is written.
//   - Acceptance, settlement and the wallet movement are separate steps in one
//     transaction, so an interruption leaves nothing half applied.
//   - A reversal whose reference has not arrived is persisted as
//     PENDING_REFERENCE with a durable retry schedule and answered 202.
func (uc *WageringUseCase) Submit(ctx context.Context, req OperationRequest) (*OperationResult, error) {
	parsed, err := parseOperation(req)
	if err != nil {
		uc.countOutcome(req.Kind, "rejected")
		return nil, err
	}

	started := time.Now()
	ctx = logging.WithTransactionID(ctx, "")
	ctx = logging.WithWalletID(ctx, req.WalletID.String())
	ctx = logging.WithProviderID(ctx, req.ProviderID)

	var result *OperationResult
	txErr := uc.tx.Do(ctx, uow.Options{
		Name:     "submit_wager_operation",
		WalletID: req.WalletID.String(),
	}, func(ctx context.Context) error {
		var err error
		result, err = uc.submitInTx(ctx, parsed)
		return err
	})
	if txErr != nil {
		uc.countOutcome(string(parsed.kind), "error")
		return nil, txErr
	}

	uc.metricsLatency(parsed.kind, req.Source, started)
	uc.countOutcome(string(parsed.kind), string(result.Status))
	return result, nil
}

// parseOperation performs every check that does not need the database. It is
// the single place the wire contract is enforced.
func parseOperation(req OperationRequest) (*validatedOperation, error) {
	operation := &validatedOperation{request: req}

	if req.IdempotencyKey == "" {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Missing idempotency key",
			"$field is required. Send {providerId}:{externalTransactionId} so a repeated delivery is recognised.",
			map[string]any{"field": "Idempotency-Key"})
	}
	if len(req.IdempotencyKey) > 255 {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Idempotency key too long",
			"$field must be at most 255 characters.", map[string]any{"field": "Idempotency-Key"})
	}
	if req.ProviderID == "" {
		return nil, xerr.Unauthenticated(xerr.CodeProviderMismatch, "Provider not authenticated",
			"The provider identity is taken from the presented credential, not from the request body.", nil)
	}
	if req.ExternalTransactionID == "" {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Missing external transaction",
			"$field is required.", map[string]any{"field": "externalTransactionId"})
	}
	if len(req.ExternalTransactionID) > 128 {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "External transaction too long",
			"$field must be at most 128 characters.", map[string]any{"field": "externalTransactionId"})
	}
	if req.PlayerID == uuid.Nil {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Missing player",
			"$field is required and must be a UUID.", map[string]any{"field": "playerId"})
	}
	if req.WalletID == uuid.Nil {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Missing wallet",
			"$field is required and must be a UUID.", map[string]any{"field": "walletId"})
	}
	if req.RoundID == "" {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Missing round",
			"$field is required so the operation can be traced to a game round.",
			map[string]any{"field": "roundId"})
	}
	if req.GameID == "" {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Missing game",
			"$field is required so the operation can be traced to a game.",
			map[string]any{"field": "gameId"})
	}

	kind, err := domain.ParseKind(req.Kind)
	if err != nil {
		return nil, err
	}
	operation.kind = kind

	currency, err := money.ParseCurrency(req.Currency)
	if err != nil {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Invalid currency",
			"$field must be an ISO 4217 alphabetic code such as BRL.",
			map[string]any{"field": "money.currency", "value": req.Currency})
	}
	amount, err := money.Parse(req.Amount, currency)
	if err != nil {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Invalid amount",
			"$field must be a decimal amount with at most two fractional digits and no exponent, such as 25.00.",
			map[string]any{"field": "money.amount", "value": req.Amount})
	}
	operation.amount = amount

	if err := kind.ValidateAmount(amount); err != nil {
		return nil, err
	}
	if kind.RequiresReference() {
		switch {
		case req.ReferenceExternalID == nil || *req.ReferenceExternalID == "":
			return nil, xerr.Validation(xerr.CodeReferenceRequired, "Reference required",
				"A $kind must name the operation it acts on through $field.",
				map[string]any{"kind": string(kind), "field": "referenceExternalTransactionId"})
		case *req.ReferenceExternalID == req.ExternalTransactionID:
			return nil, xerr.Validation(xerr.CodeReferenceRequired, "Reference cannot be itself",
				"A $kind must name a different operation as its reference.",
				map[string]any{"kind": string(kind), "field": "referenceExternalTransactionId"})
		}
	}

	operation.payload = idempotency.OfMoney(req.ProviderID, req.ExternalTransactionID, req.PlayerID,
		req.WalletID, req.RoundID, req.GameID, string(kind), amount, req.ReferenceExternalID)
	hash, err := idempotency.Hash(operation.payload)
	if err != nil {
		return nil, xerr.Infrastructure("The operation could not be fingerprinted.", err)
	}
	operation.hash = hash
	return operation, nil
}

func (uc *WageringUseCase) submitInTx(ctx context.Context, operation *validatedOperation) (*OperationResult, error) {
	request := operation.request

	// 1. The inbox claim and the work share this transaction. A message that
	//    was already handled commits nothing and replays its recorded outcome.
	if request.Inbox != nil {
		replayed, err := uc.claimInbox(ctx, request)
		if err != nil {
			return nil, err
		}
		if replayed != nil {
			return replayed, nil
		}
	}

	// 2. Idempotency, checked before anything is written.
	existing, err := uc.findByIdempotencyKey(ctx, request.ProviderID, request.IdempotencyKey)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, unavailable(err)
	}
	if existing != nil {
		return uc.replay(existing, operation)
	}

	// 3. The provider's own identity of an operation is unique: a second key
	//    cannot re-apply it.
	byExternal, err := uc.transactions.FindByExternalID(ctx, request.ProviderID, request.ExternalTransactionID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, unavailable(err)
	}
	if byExternal != nil {
		if byExternal.IdempotencyKey == request.IdempotencyKey {
			return uc.replay(byExternal, operation)
		}
		return nil, xerr.Conflict(xerr.CodeExternalTransactionConflict, "Operation already received",
			"Operation $external was already received under idempotency key $key. An operation identified by its external id cannot be applied under a second key.",
			map[string]any{"external": request.ExternalTransactionID, "key": byExternal.IdempotencyKey})
	}

	// 4. The wallet must exist, belong to the player and settle in the same
	//    currency. Checked before anything is recorded, because the index is
	//    keyed by wallet.
	wallet, err := uc.wallets.Find(ctx, request.WalletID.String())
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, xerr.NotFound(xerr.CodeWalletNotFound, "Wallet not found",
				"Wallet $wallet does not exist, so operation $external cannot be applied.",
				map[string]any{"wallet": request.WalletID.String(), "external": request.ExternalTransactionID})
		}
		return nil, unavailable(err)
	}
	if wallet.PlayerID != request.PlayerID {
		return nil, xerr.Rejected(xerr.CodeWalletNotFound, "Wallet does not belong to the player",
			"Wallet $wallet belongs to a different player than $player.",
			map[string]any{"wallet": wallet.ID.String(), "player": request.PlayerID.String()})
	}
	if wallet.Currency != string(operation.amount.Currency()) {
		return nil, xerr.Rejected(xerr.CodeCurrencyMismatch, "Currency mismatch",
			"Wallet $wallet settles in $walletCurrency but the operation is in $amountCurrency.",
			map[string]any{
				"wallet":         wallet.ID.String(),
				"walletCurrency": wallet.Currency,
				"amountCurrency": string(operation.amount.Currency()),
			})
	}

	// 5. Accept the operation in PENDING. Acceptance moves nothing.
	transactionID := uuid.New()
	ctx = logging.WithTransactionID(ctx, transactionID.String())
	// The wager transaction causes everything that follows in this operation, so
	// the causation chain is set before any command is dispatched.
	ctx = cqrs.WithCausation(ctx, transactionID.String())
	if _, err := uc.registerWager(ctx, registerwageroperation.Command{
		TransactionID:       transactionID,
		Origin:              domain.OriginExternal,
		ProviderID:          request.ProviderID,
		ExternalID:          request.ExternalTransactionID,
		IdempotencyKey:      request.IdempotencyKey,
		PayloadHash:         operation.hash,
		WalletID:            request.WalletID,
		PlayerID:            request.PlayerID,
		RoundID:             request.RoundID,
		GameID:              request.GameID,
		Kind:                operation.kind,
		Money:               operation.amount,
		ReferenceExternalID: request.ReferenceExternalID,
		CorrelationID:       logging.CorrelationID(ctx),
	}); err != nil {
		if rejection, ok := xerr.As(err); ok && rejection.Kind == xerr.KindBusinessRule {
			// A rejection raised by the domain is a decision, not a failure.
			return nil, err
		}
		return nil, err
	}
	if err := uc.pipe.Flush(ctx); err != nil {
		return nil, err
	}

	entity, err := uc.transactions.Find(ctx, transactionID.String())
	if err != nil {
		return nil, unavailable(err)
	}

	// 6. Settle, and 7. close the inbox entry — all inside the same commit.
	result, err := uc.settle(ctx, operation, entity)
	if err != nil {
		return nil, err
	}
	if request.Inbox != nil {
		if err := uc.completeInbox(ctx, request, string(result.Status)); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// settle decides the outcome of an accepted operation and applies it.
//
// The wallet movement is attempted first and the wagering transition follows
// from its result, never the other way round: the transaction cannot claim to be
// PROCESSED unless the money actually moved, and it cannot claim REJECTED for
// an overdraft the wallet would have covered.
func (uc *WageringUseCase) settle(ctx context.Context, operation *validatedOperation, entity *wagertransactiondetails.Entity) (*OperationResult, error) {
	if entity.Status == string(domain.StatusProcessed) ||
		entity.Status == string(domain.StatusRejected) ||
		entity.Status == string(domain.StatusFailed) {
		return resultOf(entity, true), nil
	}

	// The default answer is "it worked". Anything that can turn a settled
	// operation into a rejected or a waiting one has to say so explicitly, so a
	// rejection is always a decision rather than an omission.
	outcome := settlewageroperation.Outcome{Processed: true}

	// A reversal must resolve its reference before anything moves.
	reference, err := uc.resolveReference(ctx, operation, entity)
	if err != nil {
		return nil, err
	}

	switch reference.decision {
	case referenceReject:
		outcome.Processed = false
		outcome.FailureCode = reference.rejected.Code
		outcome.FailureMessage = reference.rejected.Message
	case referenceAwait:
		return uc.awaitReference(ctx, entity, reference.reason)
	default:
		// A resolved reversal carries the internal identity of the operation it
		// undid, so the ledger and the audit trail can be followed in both
		// directions.
		if reference.target != nil {
			resolved := reference.target.ID
			outcome.ResolvedReference = &resolved
		}
	}

	if outcome.Processed {
		balance, rejection := uc.applyMovement(ctx, operation, entity, reference)
		if rejection != nil {
			outcome.Processed = false
			outcome.FailureCode = rejection.Code
			outcome.FailureMessage = rejection.Message
		} else {
			outcome.BalanceAfter = balance
		}
	}

	if _, err := uc.settleWager(ctx, settlewageroperation.Command{
		TransactionID: entity.ID,
		Outcome:       outcome,
		CorrelationID: logging.CorrelationID(ctx),
		CausationID:   causationOf(outcome.ResolvedReference),
	}); err != nil {
		return nil, err
	}
	if err := uc.pipe.Flush(ctx); err != nil {
		return nil, err
	}

	settled, err := uc.transactions.Find(ctx, entity.ID.String())
	if err != nil {
		return nil, unavailable(err)
	}
	return resultOf(settled, false), nil
}

// applyMovement moves the wallet for an operation, or reports why it may not.
func (uc *WageringUseCase) applyMovement(
	ctx context.Context,
	operation *validatedOperation,
	entity *wagertransactiondetails.Entity,
	reference resolvedReference,
) (money.Money, *xerr.Error) {
	amount := operation.amount
	direction := ""
	if reference.target != nil {
		amount = reference.amount
		direction = reference.direction
	} else {
		derived, ok := operation.kind.Direction()
		if !ok {
			// A LOSS moves nothing: it records a bet that already happened and
			// changed no balance. It settles with the balance as it stands and
			// writes no ledger entry and no WalletBalanceChanged.
			balance, err := uc.currentBalance(ctx, entity.WalletID)
			if err != nil {
				return money.Money{}, xerr.Unavailable(xerr.CodeDependencyUnavailable,
					"Temporarily unavailable",
					"The wallet balance could not be read. Retry the request.", nil).WithCause(err)
			}
			return balance, nil
		}
		direction = derived
	}

	_, err := uc.moveWallet(ctx, applywalletmovement.Command{
		WalletID:      entity.WalletID,
		TransactionID: entity.ID,
		Direction:     walletDirection(direction),
		Money:         amount,
		CorrelationID: logging.CorrelationID(ctx),
		CausationID:   entity.ID.String(),
	})
	if err != nil {
		rejection, ok := xerr.As(err)
		if !ok {
			return money.Money{}, unavailableCause(err)
		}
		if rejection.Code == xerr.CodeInsufficientBalance &&
			(operation.kind == domain.KindRefund || operation.kind == domain.KindRollback) {
			// A reversal that cannot be covered is a different failure from a
			// bet that cannot be placed, and a provider must be able to tell
			// them apart.
			return money.Money{}, xerr.Rejected(xerr.CodeReversalInsufficientFunds,
				"Reversal exceeds the available balance",
				"Reversing $reference needs $amount but the wallet holds $balance. The reversal was refused so the balance is never left negative.",
				map[string]any{
					"reference": derefString(reference.target.ID.String()),
					"amount":    amount.String(),
					"kind":      string(operation.kind),
				})
		}
		return money.Money{}, rejection
	}

	// Flush so the balance is authoritative before it is reported. The
	// projection and the event share this transaction, so the row read here is
	// the state the event describes.
	if err := uc.pipe.Flush(ctx); err != nil {
		return money.Money{}, unavailableCause(err)
	}
	balance, err := uc.currentBalance(ctx, entity.WalletID)
	if err != nil {
		return money.Money{}, unavailableCause(err)
	}
	return balance, nil
}

func (uc *WageringUseCase) currentBalance(ctx context.Context, walletID uuid.UUID) (money.Money, error) {
	wallet, err := uc.wallets.Find(ctx, walletID.String())
	if err != nil {
		return money.Money{}, err
	}
	return money.FromMinor(wallet.BalanceMinor, money.Currency(wallet.Currency)), nil
}

// referenceDecision is what the settlement decided to do about a reversal's
// reference.
type referenceDecision int

const (
	referenceResolved referenceDecision = iota
	referenceAwait
	referenceReject
)

// resolvedReference is the outcome of examining a reversal's reference.
type resolvedReference struct {
	decision referenceDecision
	rejected *xerr.Error
	// target is the internal transaction the reference resolved to.
	target *wagertransactiondetails.Entity
	// amount is the value the reversal moves: the whole amount of the operation
	// it reverses.
	amount money.Money
	// direction is the wallet direction the reversal implies.
	direction string
	// reason explains a wait, for the pending-reference event.
	reason string
}

// resolveReference inspects the operation a reversal names.
//
// Providers deliver independently, so a reversal routinely arrives first. The
// three cases are distinct and each has its own outcome:
//
//   - the reference has not arrived: wait, up to a bounded number of attempts,
//     then reject with REFERENCE_NOT_FOUND;
//   - the reference arrived but is itself still pending: wait, then reject with
//     REFERENCE_NOT_PROCESSED;
//   - the reference arrived and finished without success: reject immediately
//     with REFERENCE_NOT_PROCESSED, because waiting cannot help.
func (uc *WageringUseCase) resolveReference(
	ctx context.Context,
	operation *validatedOperation,
	entity *wagertransactiondetails.Entity,
) (resolvedReference, error) {
	if !operation.kind.RequiresReference() {
		return resolvedReference{decision: referenceResolved}, nil
	}
	referenceID := *operation.request.ReferenceExternalID

	target, err := uc.transactions.FindByExternalID(ctx, operation.request.ProviderID, referenceID)
	if err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			return resolvedReference{}, unavailable(err)
		}
		if exhausted(entity.PendingAttempts, uc.cfg.MaxReferenceAttempts) {
			return resolvedReference{
				decision: referenceReject,
				rejected: xerr.Rejected(xerr.CodeReferenceNotFound, "Reference never arrived",
					"Operation $reference was not received within the retry budget, so $kind cannot be applied.",
					map[string]any{
						"reference": referenceID,
						"kind":      string(operation.kind),
						"attempts":  entity.PendingAttempts,
					}),
			}, nil
		}
		return resolvedReference{
			decision: referenceAwait,
			reason:   "the referenced operation has not been received yet",
		}, nil
	}

	if target.WalletID != entity.WalletID {
		return resolvedReference{
			decision: referenceReject,
			rejected: xerr.Rejected(xerr.CodeReferenceKindMismatch, "Reference belongs to another wallet",
				"Operation $reference acts on a different wallet than the one being reversed, so it cannot be applied.",
				map[string]any{"reference": referenceID}),
		}, nil
	}

	targetKind := target.OperationKind()
	if !kindMayReference(operation.kind, targetKind) {
		return resolvedReference{
			decision: referenceReject,
			rejected: xerr.Rejected(xerr.CodeReferenceKindMismatch, "Reference has the wrong kind",
				"A $kind can only reference a $expected operation, but $reference is a $actual.",
				map[string]any{
					"kind":      string(operation.kind),
					"reference": referenceID,
					"actual":    string(targetKind),
				}),
		}, nil
	}

	// A reversal undoes the referenced operation in full, so it moves exactly
	// the amount that operation moved. Anything else would leave the ledger
	// describing something that never happened.
	if operation.amount.AmountMinor() != target.AmountMinor ||
		operation.amount.Currency() != target.Amount().Currency() {
		return resolvedReference{
			decision: referenceReject,
			rejected: xerr.Rejected(xerr.CodeReferenceAmountMismatch, "Amount does not match the reference",
				"A $kind reverses the referenced operation in full, so its amount must be $expected. It sent $actual.",
				map[string]any{
					"kind":      string(operation.kind),
					"reference": referenceID,
					"expected":  target.Amount().String(),
					"actual":    operation.amount.String(),
				}),
		}, nil
	}

	if target.LifecycleStatus() != domain.StatusProcessed {
		if target.LifecycleStatus().Terminal() {
			return resolvedReference{
				decision: referenceReject,
				rejected: xerr.Rejected(xerr.CodeReferenceNotProcessed, "Reference did not succeed",
					"Operation $reference is $status, so there is nothing to reverse.",
					map[string]any{"reference": referenceID, "status": target.Status}),
			}, nil
		}
		if exhausted(entity.PendingAttempts, uc.cfg.MaxReferenceAttempts) {
			return resolvedReference{
				decision: referenceReject,
				rejected: xerr.Rejected(xerr.CodeReferenceNotProcessed, "Reference never completed",
					"Operation $reference is still $status after the retry budget, so $kind cannot be applied.",
					map[string]any{
						"reference": referenceID,
						"status":    target.Status,
						"kind":      string(operation.kind),
						"attempts":  entity.PendingAttempts,
					}),
			}, nil
		}
		return resolvedReference{
			decision: referenceAwait,
			reason:   fmt.Sprintf("the referenced operation is still %s", target.Status),
		}, nil
	}

	// A reference may be reversed successfully at most once. The check is here
	// for a clear rejection; the partial unique index on reversed references is
	// what makes it true regardless of the code path that gets there.
	reversal, err := uc.transactions.FindReversalOf(ctx, target.ID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return resolvedReference{}, unavailable(err)
	}
	if reversal != nil && reversal.Kind == string(operation.kind) {
		return resolvedReference{
			decision: referenceReject,
			rejected: xerr.Rejected(xerr.CodeReferenceAlreadyReversed, "Already reversed",
				"Operation $reference has already been reversed by $reversal.",
				map[string]any{"reference": referenceID, "reversal": reversal.ExternalID}),
		}, nil
	}

	direction, ok := reversalDirection(operation.kind, targetKind)
	if !ok {
		return resolvedReference{
			decision: referenceReject,
			rejected: xerr.Rejected(xerr.CodeReferenceKindMismatch, "Reference has the wrong kind",
				"A $kind cannot reverse a $actual operation.",
				map[string]any{"kind": string(operation.kind), "actual": string(targetKind)}),
		}, nil
	}

	return resolvedReference{
		decision:  referenceResolved,
		target:    target,
		amount:    target.Amount(),
		direction: direction,
	}, nil
}

// awaitReference parks the operation and schedules its next attempt.
func (uc *WageringUseCase) awaitReference(
	ctx context.Context,
	entity *wagertransactiondetails.Entity,
	reason string,
) (*OperationResult, error) {
	next := time.Now().UTC().Add(uc.referenceBackoff(entity.PendingAttempts))
	if _, err := uc.awaitReferenceCmd(ctx, awaitwagerreference.Command{
		TransactionID: entity.ID,
		NextAttemptAt: next,
		Reason:        reason,
		CorrelationID: logging.CorrelationID(ctx),
	}); err != nil {
		return nil, err
	}
	if err := uc.pipe.Flush(ctx); err != nil {
		return nil, err
	}
	updated, err := uc.transactions.Find(ctx, entity.ID.String())
	if err != nil {
		return nil, unavailable(err)
	}
	return resultOf(updated, false), nil
}

// Resume re-drives an operation left unfinished by an interrupted process.
//
// Every PENDING the database holds is durable work nobody has finished, so any
// instance may pick it up. The operation is not re-accepted: it is settled from
// the record that survived.
func (uc *WageringUseCase) Resume(ctx context.Context, entity *wagertransactiondetails.Entity) (*OperationResult, error) {
	operation := &validatedOperation{
		request: OperationRequest{
			IdempotencyKey:        entity.IdempotencyKey,
			ProviderID:            entity.ProviderID,
			ExternalTransactionID: entity.ExternalID,
			PlayerID:              entity.PlayerID,
			WalletID:              entity.WalletID,
			RoundID:               entity.RoundID,
			GameID:                entity.GameID,
			Kind:                  entity.Kind,
			Amount:                entity.Amount().String(),
			Currency:              entity.Currency,
			ReferenceExternalID:   optionalString(entity.ReferenceExternal),
			Source:                SourceResume,
		},
		kind:   entity.OperationKind(),
		amount: entity.Amount(),
		hash:   entity.PayloadHash,
	}

	ctx = logging.WithTransactionID(ctx, entity.ID.String())
	ctx = logging.WithWalletID(ctx, entity.WalletID.String())
	ctx = logging.WithProviderID(ctx, entity.ProviderID)

	var result *OperationResult
	err := uc.tx.Do(ctx, uow.Options{
		Name:     "resume_wager_operation",
		WalletID: entity.WalletID.String(),
	}, func(ctx context.Context) error {
		fresh, err := uc.transactions.Find(ctx, entity.ID.String())
		if err != nil {
			return unavailable(err)
		}
		result, err = uc.settle(ctx, operation, fresh)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// DuePending lists the operations waiting for a reference whose next attempt is
// due.
func (uc *WageringUseCase) DuePending(ctx context.Context, limit int) ([]*wagertransactiondetails.Entity, error) {
	entities, err := uc.transactions.DuePending(ctx, limit)
	if err != nil {
		return nil, unavailable(err)
	}
	return entities, nil
}

// claimInbox records the broker identity of the message.
//
// It returns a result when the message was already handled, in which case the
// caller must not do the work again.
func (uc *WageringUseCase) claimInbox(ctx context.Context, request OperationRequest) (*OperationResult, error) {
	claimed, err := uc.inboxRepo.Claim(ctx, inbox.Record{
		ConsumerName: request.Inbox.ConsumerName,
		MessageID:    request.Inbox.MessageID,
		PayloadHash:  request.Inbox.PayloadHash,
		ReceivedAt:   request.Inbox.ReceivedAt,
	})
	if err != nil {
		return nil, unavailable(err)
	}
	if claimed {
		return nil, nil
	}

	existing, err := uc.findByIdempotencyKey(ctx, request.ProviderID, request.IdempotencyKey)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// The claim exists but no operation does: the previous attempt died
			// between claiming and committing, which cannot happen inside one
			// transaction — so the row was left by a version that did. Treat it
			// as not claimed and let the work proceed; the unique index below
			// still prevents a duplicate.
			return nil, xerr.Unavailable(xerr.CodeDependencyUnavailable, "Temporarily unavailable",
				"The message is being retried. Retry shortly.", nil)
		}
		return nil, unavailable(err)
	}
	uc.countDuplicate("inbox")
	return resultOf(existing, true), nil
}

func (uc *WageringUseCase) completeInbox(ctx context.Context, request OperationRequest, outcome string) error {
	if err := uc.inboxRepo.Complete(ctx, request.Inbox.ConsumerName, request.Inbox.MessageID, outcome, time.Now().UTC()); err != nil {
		return unavailable(err)
	}
	return nil
}

func (uc *WageringUseCase) findByIdempotencyKey(ctx context.Context, providerID, key string) (*wagertransactiondetails.Entity, error) {
	entity, err := uc.transactions.FindByIdempotencyKey(ctx, providerID, key)
	if err != nil {
		return nil, err
	}
	uc.countDuplicate("idempotency_key")
	return entity, nil
}

// replay returns the stored outcome of an operation already applied.
func (uc *WageringUseCase) replay(existing *wagertransactiondetails.Entity, operation *validatedOperation) (*OperationResult, error) {
	if existing.PayloadHash != operation.hash {
		return nil, xerr.Conflict(xerr.CodeIdempotencyKeyConflict, "Idempotency key reused with a different payload",
			"Idempotency key $key was already used for a different operation. Use a new key, or resend the original payload.",
			map[string]any{"key": operation.request.IdempotencyKey, "external": existing.ExternalID})
	}
	return resultOf(existing, true), nil
}

// resultOf renders a persisted transaction as the answer a caller expects.
func resultOf(entity *wagertransactiondetails.Entity, replay bool) *OperationResult {
	result := &OperationResult{
		TransactionID:     entity.ID,
		Status:            entity.Status,
		Balance:           moneyView(entity.BalanceAfter()),
		FailureCode:       xerr.Code(entity.FailureCode),
		FailureMessage:    entity.FailureMessage,
		IdempotentReplay:  replay,
		ReferenceExternal: entity.ReferenceExternal,
	}
	return result
}

// validatedOperation is a request that passed every check that does not need
// the database.
type validatedOperation struct {
	request OperationRequest
	kind    domain.Kind
	amount  money.Money
	payload idempotency.Payload
	hash    string
}

func (uc *WageringUseCase) referenceBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := uc.cfg.ReferenceBackoffBase
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= uc.cfg.ReferenceBackoffMax {
			return uc.cfg.ReferenceBackoffMax
		}
	}
	if delay > uc.cfg.ReferenceBackoffMax {
		return uc.cfg.ReferenceBackoffMax
	}
	return delay
}

func exhausted(attempts, max int) bool { return attempts >= max }

// kindMayReference reports whether kind may act on referenced.
func kindMayReference(kind, referenced domain.Kind) bool {
	if kind == domain.KindRefund {
		return domain.IsRefundable(referenced)
	}
	if kind == domain.KindRollback {
		return domain.IsReversible(referenced)
	}
	return false
}

// reversalDirection returns the wallet direction a reversal implies: the
// opposite of what the referenced operation did.
func reversalDirection(kind, referenced domain.Kind) (string, bool) {
	switch kind {
	case domain.KindRefund:
		// A refund returns a bet's debit to the wallet.
		if domain.IsRefundable(referenced) {
			return string(wdomain.DirectionCredit), true
		}
	case domain.KindRollback:
		// Rolling back undoes the referenced movement, whatever it was.
		if referenced == domain.KindBet {
			return string(wdomain.DirectionCredit), true
		}
		if domain.IsReversible(referenced) {
			return string(wdomain.DirectionDebit), true
		}
	}
	return "", false
}

func walletDirection(direction string) wdomain.Direction {
	return wdomain.Direction(direction)
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func derefString(value string) string { return value }

// unavailable classifies a dependency failure so a caller can retry it rather
// than treat it as a decision.
func unavailable(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := xerr.As(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return unavailableCause(err)
}

func (uc *WageringUseCase) countOutcome(kind, outcome string) {
	if uc.metrics != nil {
		uc.metrics.OperationResults.WithLabelValues(orUnknown(kind), outcome).Inc()
	}
}

func (uc *WageringUseCase) countDuplicate(source string) {
	if uc.metrics != nil {
		uc.metrics.IdempotentReplays.WithLabelValues("operation", source).Inc()
	}
}

func (uc *WageringUseCase) metricsLatency(kind domain.Kind, source Source, started time.Time) {
	if uc.metrics != nil {
		uc.metrics.OperationLatency.
			WithLabelValues(string(kind), string(source)).
			Observe(time.Since(started).Seconds())
	}
}

// unavailableCause marks a dependency failure as retryable.
func causationOf(reference *uuid.UUID) string {
	if reference == nil {
		return ""
	}
	return reference.String()
}

func unavailableCause(err error) *xerr.Error {
	return xerr.Unavailable(xerr.CodeDependencyUnavailable, "Temporarily unavailable",
		"A dependency of this operation is unavailable. Retry with the same idempotency key.", nil).
		WithCause(err)
}

func orUnknown(kind string) string {
	if kind == "" {
		return "unknown"
	}
	return kind
}
