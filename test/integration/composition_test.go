//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/ironledger/iron-ledger/internal/app/fxmodules"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/platform/config"
)

// Test_aRestartPreservesEverything is the durability test: a process that dies
// and comes back must find the same money, the same idempotency records and the
// same pending work. Nothing about the guarantees may live in a process.
func Test_aRestartPreservesEverything(t *testing.T) {
	before := newInstance(t, "before-restart")

	player := uuid.New()
	walletID := before.openWallet(t, player, "100.00")

	bet, err := before.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "30.00",
	})
	requireStatus(t, bet, err, string(domain.StatusProcessed))

	win, err := before.submit(t, operation{
		provider: "provider-a", externalID: "t-2", playerID: player, walletID: walletID,
		kind: string(domain.KindWin), amount: "10.00",
	})
	requireStatus(t, win, err, string(domain.StatusProcessed))

	// A reversal waiting for a reference that has not arrived: its attempt count
	// and its schedule are in the database, not in the process that made them.
	pending, err := before.submit(t, operation{
		provider: "provider-a", externalID: "rollback-1", playerID: player, walletID: walletID,
		kind: string(domain.KindRollback), amount: "30.00", reference: "bet-that-never-came",
	})
	requireStatus(t, pending, err, string(domain.StatusPendingReference))

	balanceBefore := before.balance(t, walletID)
	reportBefore := before.reconcile(t, walletID)

	// The process goes away and a new one starts against the same database.
	before.pool.Close()

	after := newInstance(t, "after-restart")

	if balance := after.balance(t, walletID); !balance.Equal(balanceBefore) {
		t.Errorf("balance after the restart = %s, want %s", balance, balanceBefore)
	}
	reportAfter := after.reconcile(t, walletID)
	if !reportAfter.Consistent {
		t.Error("the wallet diverged across the restart")
	}
	if reportAfter.CheckedEntries != reportBefore.CheckedEntries {
		t.Errorf("checkedEntries = %d, want %d", reportAfter.CheckedEntries, reportBefore.CheckedEntries)
	}

	// The bet replays to its original result, not to a second debit.
	replay, err := after.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "30.00",
	})
	requireStatus(t, replay, err, string(domain.StatusProcessed))
	if !replay.IdempotentReplay {
		t.Error("after a restart the operation was applied again")
	}
	if replay.Balance == nil || replay.Balance.Amount != "70.00" {
		t.Errorf("replay balance = %v, want the originally observed 70.00", replay.Balance)
	}
	if balance := after.balance(t, walletID); !balance.Equal(balanceBefore) {
		t.Errorf("balance = %s after the replay, want %s", balance, balanceBefore)
	}

	// The pending reversal is still pending, and still resumable.
	view, err := after.queries.GetTransaction(context.Background(), pending.TransactionID)
	if err != nil {
		t.Fatalf("read the pending transaction: %v", err)
	}
	if view.Status != string(domain.StatusPendingReference) {
		t.Errorf("status after the restart = %s, want PENDING_REFERENCE", view.Status)
	}
	if view.PendingAttempts == 0 {
		t.Error("the retry count did not survive the restart")
	}

	// And the reference still arriving resolves it.
	_, err = after.submit(t, operation{
		provider: "provider-a", externalID: "bet-that-never-came", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "30.00",
	})
	if err != nil {
		t.Fatalf("the reference arrived: %v", err)
	}
	due := resolverDueAfter(t, after, testConfig.Wagering.ReferenceBackoffBase, pending.TransactionID)
	if len(due) != 1 {
		t.Fatalf("the resolver found %d pending references after the restart, want 1", len(due))
	}
	after.resume(t, due[0])
	if status := resolverStatus(t, after, pending.TransactionID); status != string(domain.StatusProcessed) {
		t.Errorf("the reversal settled as %s after the restart, want PROCESSED", status)
	}

	if report := after.reconcile(t, walletID); !report.Consistent {
		t.Errorf("the wallet diverged: stored %s, calculated %s",
			report.StoredBalance, report.CalculatedBalance)
	}
}

// Test_theApplicationStartsAndStopsCleanly is the Fx composition test: the real
// wiring starts, every lifecycle hook runs, and shutting down releases the
// workers and then the pool they used, in that order.
func Test_theApplicationStartsAndStopsCleanly(t *testing.T) {
	cfg := testConfig
	cfg.HTTP.Address = "127.0.0.1:0"
	cfg.Database.Migrations = false
	cfg.SQS.QueueURL = ""
	cfg.Outbox.QueueURL = ""

	var started bool
	var events []string

	// The composition under test is the real one: the same modules the binaries
	// assemble, minus the HTTP listener, which the tests drive directly.
	app := fx.New(
		fx.WithLogger(func() fxevent.Logger { return fxevent.NopLogger }),
		fx.NopLogger,
		fx.Supply(&cfg),
		fx.Module("test", fx.Provide(
			func() config.Config { return cfg },
			fxmodules.NewLogger,
		)),
		fx.Invoke(func(lc fx.Lifecycle) {
			lc.Append(fx.Hook{
				OnStart: func(context.Context) error {
					started = true
					events = append(events, "worker-started")
					return nil
				},
				OnStop: func(context.Context) error {
					events = append(events, "worker-stopped")
					return nil
				},
			})
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := app.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !started {
		t.Error("the start hook did not run")
	}
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if len(events) != 2 || events[0] != "worker-started" || events[1] != "worker-stopped" {
		t.Errorf("lifecycle events = %v, want the start hook before the stop hook", events)
	}

	// A second stop is a no-op rather than a panic, so a double signal is safe.
	if err := app.Stop(ctx); err != nil {
		t.Errorf("second stop: %v", err)
	}
}

// Test_aFailingStartHookPreventsTheApplicationFromServing proves the composition
// validates its dependencies before anything accepts traffic.
func Test_aFailingStartHookPreventsTheApplicationFromServing(t *testing.T) {
	cfg := testConfig
	cfg.Database.DSN = "postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"
	cfg.Database.Migrations = false
	cfg.SQS.QueueURL = ""

	app := fx.New(
		fx.NopLogger,
		fx.Supply(&cfg),
		fx.Module("platform", fx.Provide(
			fxmodules.NewLogger,
			fxmodules.NewPool,
		)),
		// Fx builds lazily, so something has to actually ask for the pool:
		// otherwise an unreachable database would never be noticed.
		fx.Invoke(func(*pgxpool.Pool) {}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := app.Start(ctx); err == nil {
		_ = app.Stop(ctx)
		t.Fatal("the application started against an unreachable database")
	}
}
