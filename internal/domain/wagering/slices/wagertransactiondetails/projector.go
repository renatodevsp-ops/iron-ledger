package wagertransactiondetails

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/domain/wagering/events"
	"github.com/ironledger/iron-ledger/internal/platform/pgdb"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// Projector maintains the wager transaction index.
//
// It is driven from the same transaction that appended the events, so the row
// and the stream can never disagree: either both are durable or neither is.
type Projector struct {
	resolver pgdb.Resolver
}

// NewProjector builds the projector.
func NewProjector(resolver pgdb.Resolver) *Projector { return &Projector{resolver: resolver} }

// NewGroup returns the projector as an event group processor.
func NewGroup(resolver pgdb.Resolver) *cqrs.EventGroupProcessor {
	p := NewProjector(resolver)
	return cqrs.NewEventGroupProcessor(
		cqrs.OnEvent(p.OnRegistered),
		cqrs.OnEvent(p.OnProcessed),
		cqrs.OnEvent(p.OnRejected),
		cqrs.OnEvent(p.OnPendingReference),
		cqrs.OnEvent(p.OnFailed),
	)
}

// OnRegistered accepts the operation into the index in PENDING.
func (p *Projector) OnRegistered(ctx context.Context, event *events.WagerTransactionRegistered) error {
	const insert = `
		INSERT INTO ` + TableName + ` (
			id, origin, provider_id, external_transaction_id, idempotency_key, payload_hash,
			wallet_id, player_id, round_id, game_id, kind, amount_minor, currency,
			reference_external_id, status, pending_attempts, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'PENDING',0,$15,$15)
		ON CONFLICT (id) DO NOTHING`
	_, err := p.resolver.Q(ctx).Exec(ctx, insert,
		event.TransactionID, string(event.Origin), nullIfEmpty(event.ProviderID),
		nullIfEmpty(event.ExternalID), nullIfEmpty(event.IdempotencyKey), event.PayloadHash,
		event.WalletID, event.PlayerID, nullIfEmpty(event.RoundID), nullIfEmpty(event.GameID),
		string(event.Kind), event.Money.AmountMinor(), string(event.Money.Currency()),
		nullIfEmptyPtr(event.ReferenceExtern), occurredAt(ctx),
	)
	if err != nil {
		return fmt.Errorf("wagertransactiondetails: insert transaction: %w", err)
	}
	return nil
}

// OnProcessed records the settled outcome and the balance observed then.
func (p *Projector) OnProcessed(ctx context.Context, event *events.WagerTransactionProcessed) error {
	const update = `
		UPDATE ` + TableName + ` SET
			status = 'PROCESSED',
			balance_after_minor = $2,
			balance_after_currency = $3,
			reference_transaction_id = $4,
			reference_resolved_at = $5,
			failure_code = NULL,
			failure_message = NULL,
			next_attempt_at = NULL,
			updated_at = $6
		WHERE id = $1 AND status IN ('PENDING','PENDING_REFERENCE')`
	tag, err := p.resolver.Q(ctx).Exec(ctx, update,
		event.TransactionID, event.BalanceAfter.AmountMinor(), string(event.BalanceAfter.Currency()),
		event.ResolvedReference, nullTime(ctx, event.ResolvedReference != nil), occurredAt(ctx),
	)
	if err != nil {
		if pgdb.IsUniqueViolation(err) {
			// The referenced operation was already reversed successfully. The
			// partial unique index on reversed references is what makes a
			// double reversal impossible, whatever the code path.
			return xerr.Rejected(xerr.CodeReferenceAlreadyReversed, "Reference already reversed",
				"Operation $reference has already been reversed successfully; reversing it again would return money twice.",
				map[string]any{"reference": uuidOrEmpty(event.ResolvedReference)})
		}
		return fmt.Errorf("wagertransactiondetails: mark processed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("wagertransactiondetails: transaction %s is not pending", event.TransactionID)
	}
	return nil
}

// OnRejected records a terminal business rejection.
func (p *Projector) OnRejected(ctx context.Context, event *events.WagerTransactionRejected) error {
	const update = `
		UPDATE ` + TableName + ` SET
			status = 'REJECTED', failure_code = $2, failure_message = $3,
			next_attempt_at = NULL, updated_at = $4
		WHERE id = $1 AND status IN ('PENDING','PENDING_REFERENCE')`
	if _, err := p.resolver.Q(ctx).Exec(ctx, update,
		event.TransactionID, string(event.FailureCode), event.FailureMessage, occurredAt(ctx),
	); err != nil {
		return fmt.Errorf("wagertransactiondetails: mark rejected: %w", err)
	}
	return nil
}

// OnPendingReference records that the operation is waiting, with the schedule
// the reference worker will follow.
func (p *Projector) OnPendingReference(ctx context.Context, event *events.WagerTransactionPendingReference) error {
	const update = `
		UPDATE ` + TableName + ` SET
			status = 'PENDING_REFERENCE',
			pending_attempts = $2,
			next_attempt_at = $3,
			failure_message = $4,
			updated_at = $5
		WHERE id = $1 AND status IN ('PENDING','PENDING_REFERENCE')`
	_, err := p.resolver.Q(ctx).Exec(ctx, update,
		event.TransactionID, event.Attempt, event.NextAttemptAt.UTC(), event.Reason, occurredAt(ctx),
	)
	if err != nil {
		return fmt.Errorf("wagertransactiondetails: mark pending reference: %w", err)
	}
	return nil
}

// OnFailed records a permanent infrastructure failure.
func (p *Projector) OnFailed(ctx context.Context, event *events.WagerTransactionFailed) error {
	const update = `
		UPDATE ` + TableName + ` SET
			status = 'FAILED', failure_code = $2, failure_message = $3,
			next_attempt_at = NULL, updated_at = $4
		WHERE id = $1 AND status IN ('PENDING','PENDING_REFERENCE')`
	if _, err := p.resolver.Q(ctx).Exec(ctx, update,
		event.TransactionID, string(event.FailureCode), event.FailureMessage, occurredAt(ctx),
	); err != nil {
		return fmt.Errorf("wagertransactiondetails: mark failed: %w", err)
	}
	return nil
}

func occurredAt(ctx context.Context) time.Time {
	if v := cqrs.OccurredAtFromContext(ctx); !v.IsZero() {
		return v.UTC()
	}
	return time.Now().UTC()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfEmptyPtr(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

// nullTime resolves to the instant of the event being projected, or to NULL
// when the column does not apply. It reads the event's own timestamp rather
// than the wall clock so that replaying the stream reproduces the row that was
// originally written instead of stamping a new resolution time every time.
func nullTime(ctx context.Context, when bool) any {
	if !when {
		return nil
	}
	return occurredAt(ctx)
}

func uuidOrEmpty(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}
