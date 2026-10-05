package walletdetails

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/domain/wallet/events"
	"github.com/ironledger/iron-ledger/internal/platform/pgdb"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// Projector maintains the wallets row.
//
// The balance is written with a conditional update: the row only moves when it
// still carries the version the event was derived from. Together with the
// CHECK (balance_minor >= 0) constraint and the unique (player_id, currency)
// index, that makes a lost update or a duplicated credit impossible even if
// two processes disagree about the state of the world.
type Projector struct {
	resolver pgdb.Resolver
}

// NewProjector builds the projector over a query resolver.
func NewProjector(resolver pgdb.Resolver) *Projector {
	return &Projector{resolver: resolver}
}

// NewGroup returns the projector as an event group processor, ready to be
// driven by the in-transaction dispatcher.
func NewGroup(resolver pgdb.Resolver) *cqrs.EventGroupProcessor {
	p := NewProjector(resolver)
	return cqrs.NewEventGroupProcessor(
		cqrs.OnEvent(p.OnWalletOpened),
		cqrs.OnEvent(p.OnWalletBalanceChanged),
	)
}

// OnWalletOpened creates the wallet row at its post-opening state.
//
// A wallet opened with a positive balance is already credited at version 1, and
// the WalletBalanceChanged that follows in the same stream describes that same
// credit. The row is therefore written once, at its final balance, and the
// balance-change handler recognises the change as already applied instead of
// applying it a second time.
func (p *Projector) OnWalletOpened(ctx context.Context, event *events.WalletOpened) error {
	const insert = `
		INSERT INTO ` + TableName + ` (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		ON CONFLICT (id) DO NOTHING`
	if _, err := p.resolver.Q(ctx).Exec(ctx, insert,
		event.WalletID, event.PlayerID, string(event.Balance.Currency()),
		event.Balance.AmountMinor(), int64(1), occurredAt(ctx),
	); err != nil {
		if pgdb.IsUniqueViolation(err) {
			return xerr.Conflict(xerr.CodeWalletAlreadyExists, "Wallet already exists",
				"A wallet is already open for player $player in $currency.",
				map[string]any{"player": event.PlayerID.String(), "currency": string(event.Balance.Currency())})
		}
		return fmt.Errorf("walletdetails: insert wallet: %w", err)
	}
	return nil
}

// OnWalletBalanceChanged applies a balance movement, refusing to overwrite a
// balance that moved underneath it.
func (p *Projector) OnWalletBalanceChanged(ctx context.Context, event *events.WalletBalanceChanged) error {
	after := event.BalanceAfter.AmountMinor()
	if after < 0 {
		// Belt and braces: the aggregate already refuses this, and so does the
		// CHECK constraint. Refusing here means the projection never depends on
		// the constraint being present.
		return xerr.Rejected(xerr.CodeInsufficientBalance, "Insufficient balance",
			"The movement would leave wallet $wallet negative.",
			map[string]any{"wallet": event.WalletID.String()})
	}

	// The row moves only when it is still at the version this event was derived
	// from and does not already carry the resulting balance. Losing that race is
	// a lost update, and the database — not a local mutex — is what decides it.
	const update = `
		UPDATE ` + TableName + ` SET
			balance_minor = $1, version = $2, updated_at = $3
		 WHERE id = $4 AND version = $5 AND balance_minor <> $1`
	tag, err := p.resolver.Q(ctx).Exec(ctx, update,
		after, event.WalletVersion, occurredAt(ctx), event.WalletID, event.WalletVersion-1,
	)
	if err != nil {
		return fmt.Errorf("walletdetails: update balance: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	// The row did not move. Either this exact change is already recorded — a
	// converging projection, which happens when a wallet is opened with a
	// balance, because WalletOpened already wrote the final state — or somebody
	// moved the wallet underneath this event. The first is harmless; the second
	// means the whole transaction must be retried and the command decided again
	// against the current stream.
	observedVersion, observedBalance, found, err := p.observe(ctx, event.WalletID)
	if err != nil {
		return err
	}
	if found && observedVersion == event.WalletVersion && observedBalance == after {
		return nil
	}
	return fmt.Errorf("%w: wallet %s expected version %d balance %d, found version %d balance %d",
		uow.ErrConcurrent, event.WalletID, event.WalletVersion-1, after, observedVersion, observedBalance)
}

// observe reads the wallet row as it currently stands, so a refused update can
// say what it found instead of only what it expected.
func (p *Projector) observe(ctx context.Context, walletID uuid.UUID) (version, balance int64, found bool, err error) {
	const current = `SELECT version, balance_minor FROM ` + TableName + ` WHERE id = $1`
	err = p.resolver.Q(ctx).QueryRow(ctx, current, walletID).Scan(&version, &balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("walletdetails: read wallet state: %w", err)
	}
	return version, balance, true, nil
}

// occurredAt prefers the instant of the event being projected, so a replay
// reproduces the original timestamps instead of inventing new ones.
func occurredAt(ctx context.Context) time.Time {
	if v := cqrs.OccurredAtFromContext(ctx); !v.IsZero() {
		return v.UTC()
	}
	return time.Now().UTC()
}
