package walletledgerentries

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/domain/wallet/domain"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/events"
	"github.com/ironledger/iron-ledger/internal/platform/pgdb"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// Projector appends one entry per effective balance change.
//
// The insert is idempotent on (wallet_id, transaction_id): a wallet may only be
// moved once per wager transaction, and the database refuses to say otherwise.
// A duplicate insert is treated as convergence, not as an error, so replaying a
// projection cannot corrupt the ledger — and the second one is still visible as
// a conflict if someone tries to move a wallet twice on purpose.
type Projector struct {
	resolver pgdb.Resolver
}

// NewProjector builds the projector.
func NewProjector(resolver pgdb.Resolver) *Projector { return &Projector{resolver: resolver} }

// NewGroup returns the projector as an event group processor.
func NewGroup(resolver pgdb.Resolver) *cqrs.EventGroupProcessor {
	p := NewProjector(resolver)
	return cqrs.NewEventGroupProcessor(
		cqrs.OnEvent(p.OnWalletOpened),
		cqrs.OnEvent(p.OnWalletBalanceChanged),
	)
}

// OnWalletOpened records the opening credit, when the wallet was opened with a
// positive balance. A wallet opened at zero leaves no entry: there was no
// movement.
func (p *Projector) OnWalletOpened(ctx context.Context, event *events.WalletOpened) error {
	if !event.Balance.IsPositive() {
		return nil
	}
	return p.append(ctx, entry{
		ID:            event.OpeningTx,
		WalletID:      event.WalletID,
		TransactionID: event.OpeningTx,
		Direction:     domain.DirectionCredit,
		Amount:        event.Balance,
		Before:        money.Zero(event.Balance.Currency()),
		After:         event.Balance,
	})
}

// OnWalletBalanceChanged records the movement itself.
func (p *Projector) OnWalletBalanceChanged(ctx context.Context, event *events.WalletBalanceChanged) error {
	return p.append(ctx, entry{
		ID:            event.TransactionID,
		WalletID:      event.WalletID,
		TransactionID: event.TransactionID,
		Direction:     event.Direction,
		Amount:        event.Money,
		Before:        event.BalanceBefore,
		After:         event.BalanceAfter,
	})
}

type entry struct {
	ID            uuid.UUID
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     domain.Direction
	Amount        money.Money
	Before        money.Money
	After         money.Money
}

func (p *Projector) append(ctx context.Context, e entry) error {
	if !e.After.IsNegative() {
		// The invariant the entry claims: balanceAfter = balanceBefore ± amount.
		expected, err := apply(e)
		if err != nil {
			return err
		}
		if expected != e.After.AmountMinor() {
			return xerr.Infrastructure(
				"ledger entry does not balance: the event and the entry disagree",
				fmt.Errorf("before=%d %s%c amount=%d after=%d",
					e.Before.AmountMinor(), e.Direction, '+', e.Amount.AmountMinor(), e.After.AmountMinor()),
			)
		}
	}

	const insert = `
		INSERT INTO ` + TableName + `
			(id, wallet_id, transaction_id, direction, amount_minor,
			 balance_before_minor, balance_after_minor, currency, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (wallet_id, transaction_id) DO NOTHING`
	_, err := p.resolver.Q(ctx).Exec(ctx, insert,
		e.ID, e.WalletID, e.TransactionID, string(e.Direction), e.Amount.AmountMinor(),
		e.Before.AmountMinor(), e.After.AmountMinor(), string(e.Amount.Currency()), occurredAt(ctx),
	)
	if err != nil {
		return fmt.Errorf("walletledgerentries: append entry: %w", err)
	}
	return nil
}

func apply(e entry) (int64, error) {
	if e.Direction == domain.DirectionCredit {
		sum, err := e.Before.Add(e.Amount)
		if err != nil {
			return 0, xerr.Infrastructure("ledger entry does not balance", err)
		}
		return sum.AmountMinor(), nil
	}
	diff, err := e.Before.Sub(e.Amount)
	if err != nil {
		return 0, xerr.Infrastructure("ledger entry does not balance", err)
	}
	return diff.AmountMinor(), nil
}

func occurredAt(ctx context.Context) time.Time {
	if v := cqrs.OccurredAtFromContext(ctx); !v.IsZero() {
		return v.UTC()
	}
	return time.Now().UTC()
}
