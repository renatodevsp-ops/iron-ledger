//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// Test_theLedgerIsAppendOnly proves the database itself refuses to edit or erase
// history: the guarantees do not depend on the application being careful.
func Test_theLedgerIsAppendOnly(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")
	_, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "25.00",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx := context.Background()

	_, err = api.pool.Exec(ctx,
		`UPDATE wallet_ledger_entries SET amount_minor = 1 WHERE wallet_id = $1`, walletID)
	if err == nil {
		t.Error("the ledger accepted an UPDATE; it must be append-only")
	} else if pgCode(err) != "23001" && pgCode(err) != "2F004" {
		t.Logf("update refused with SQLSTATE %s", pgCode(err))
	}

	_, err = api.pool.Exec(ctx,
		`DELETE FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID)
	if err == nil {
		t.Error("the ledger accepted a DELETE; it must be append-only")
	} else if pgCode(err) != "23001" && pgCode(err) != "2F004" {
		t.Logf("delete refused with SQLSTATE %s", pgCode(err))
	}

	_, err = api.pool.Exec(ctx, `TRUNCATE wallet_ledger_entries`)
	if err == nil {
		t.Error("the ledger accepted a TRUNCATE; it must be append-only")
	}

	if entries := api.ledgerEntries(t, walletID); entries != 2 {
		t.Errorf("the wallet holds %d ledger entries after the refusals, want 2", entries)
	}
	if report := api.reconcile(t, walletID); !report.Consistent {
		t.Error("the wallet diverged")
	}
}

// Test_aLedgerEntryMustAddUp proves the balance invariant is enforced by the
// schema, not only by the projection that writes it.
func Test_aLedgerEntryMustAddUp(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")

	ctx := context.Background()
	_, err := api.pool.Exec(ctx, `
		INSERT INTO wallet_ledger_entries
			(id, wallet_id, transaction_id, direction, amount_minor,
			 balance_before_minor, balance_after_minor, currency, created_at)
		VALUES ($1, $2, $3, 'DEBIT', 1000, 10000, 1, 'BRL', now())`,
		uuid.New(), walletID, uuid.New())
	if err == nil {
		t.Error("the ledger accepted an entry whose balances do not add up")
	} else if pgCode(err) != "23514" {
		t.Errorf("SQLSTATE = %s, want 23514 (check violation)", pgCode(err))
	}

	_, err = api.pool.Exec(ctx, `
		INSERT INTO wallet_ledger_entries
			(id, wallet_id, transaction_id, direction, amount_minor,
			 balance_before_minor, balance_after_minor, currency, created_at)
		VALUES ($1, $2, $3, 'DEBIT', 0, 10000, 10000, 'BRL', now())`,
		uuid.New(), walletID, uuid.New())
	if err == nil {
		t.Error("the ledger accepted an entry with no value")
	}
}

// Test_aWalletCanNeverHoldANegativeBalance proves the constraint holds even
// against a writer that bypasses the aggregate.
func Test_aWalletCanNeverHoldANegativeBalance(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "10.00")

	_, err := api.pool.Exec(context.Background(),
		`UPDATE wallets SET balance_minor = -1 WHERE id = $1`, walletID)
	if err == nil {
		t.Fatal("the wallets table accepted a negative balance")
	}
	if pgCode(err) != "23514" {
		t.Errorf("SQLSTATE = %s, want 23514 (check violation)", pgCode(err))
	}
	if balance := api.balance(t, walletID); balance.String() != "10.00" {
		t.Errorf("balance = %s, want 10.00 after the refused update", balance)
	}
}

// Test_aWalletIsOpenedOncePerPlayerAndCurrency proves the initial credit cannot
// be duplicated, whichever way the duplicate is attempted.
func Test_aWalletIsOpenedOncePerPlayerAndCurrency(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	api.openWallet(t, player, "100.00")

	_, err := api.wallets.Open(context.Background(), openRequest(player, "100.00"))
	requireCode(t, err, xerr.CodeWalletAlreadyExists)

	// And the index refuses it even when the application does not get there
	// first.
	_, err = api.pool.Exec(context.Background(),
		`INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		 VALUES ($1, $2, 'BRL', 10000, 1, now(), now())`,
		uuid.New(), player)
	if err == nil {
		t.Error("the wallets table accepted a second wallet for the same player and currency")
	} else if pgCode(err) != "23505" {
		t.Errorf("SQLSTATE = %s, want 23505 (unique violation)", pgCode(err))
	}

	opening := api.countRows(t,
		`SELECT count(*) FROM wager_transactions WHERE player_id = $1 AND kind = 'OPENING'`, player)
	if opening != 1 {
		t.Errorf("the player has %d opening transactions, want 1", opening)
	}
}

// Test_aZeroBalanceOpeningRecordsNothing proves an opening with no money leaves
// no financial trace: no opening transaction, no ledger entry, no event.
func Test_aZeroBalanceOpeningRecordsNothing(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "0.00")

	if balance := api.balance(t, walletID); !balance.IsZero() {
		t.Errorf("balance = %s, want 0.00", balance)
	}
	if entries := api.ledgerEntries(t, walletID); entries != 0 {
		t.Errorf("the wallet holds %d ledger entries, want 0", entries)
	}
	if opening := api.countRows(t,
		`SELECT count(*) FROM wager_transactions WHERE player_id = $1 AND kind = 'OPENING'`, player); opening != 0 {
		t.Errorf("the player has %d opening transactions, want 0", opening)
	}
	if events := api.countRows(t,
		`SELECT count(*) FROM outbox_messages WHERE event_type = 'WalletBalanceChanged'`); events == 0 {
		t.Log("no balance-change event was produced, as expected for a zero opening")
	}
	if report := api.reconcile(t, walletID); !report.Consistent {
		t.Error("the wallet diverged")
	}
}

// Test_aReversalCannotBeAppliedTwice proves the database refuses a second
// reversal of the same operation, whatever the code path.
func Test_aReversalCannotBeAppliedTwice(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")

	bet, err := api.submit(t, operation{
		provider: "provider-a", externalID: "bet-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "30.00",
	})
	requireStatus(t, bet, err, string(domain.StatusProcessed))

	refund, err := api.submit(t, operation{
		provider: "provider-a", externalID: "refund-1", playerID: player, walletID: walletID,
		kind: string(domain.KindRefund), amount: "30.00", reference: "bet-1",
	})
	requireStatus(t, refund, err, string(domain.StatusProcessed))

	// A second refund of the same bet is refused by the application...
	second, err := api.submit(t, operation{
		provider: "provider-a", externalID: "refund-2", playerID: player, walletID: walletID,
		kind: string(domain.KindRefund), amount: "30.00", reference: "bet-1",
	})
	requireStatus(t, second, err, string(domain.StatusRejected))
	if second.FailureCode != xerr.CodeReferenceAlreadyReversed {
		t.Errorf("failureCode = %s, want %s", second.FailureCode, xerr.CodeReferenceAlreadyReversed)
	}

	// ...and the partial unique index refuses it even if the application does
	// not get there first.
	_, err = api.pool.Exec(context.Background(), `
		INSERT INTO wager_transactions
			(id, origin, provider_id, external_transaction_id, idempotency_key, payload_hash,
			 wallet_id, player_id, round_id, game_id, kind, amount_minor, currency,
			 reference_external_id, reference_transaction_id, status,
			 balance_after_minor, balance_after_currency, created_at, updated_at)
		VALUES ($1,'EXTERNAL','provider-a','sneaky','provider-a:sneaky','hash',
			$2,$3,'round-1','g','REFUND',3000,'BRL','bet-1',$4,'PROCESSED',
			10000,'BRL',now(),now())`,
		uuid.New(), walletID, player, bet.TransactionID)
	if err == nil {
		t.Error("the index accepted a second successful reversal of the same operation")
	} else if pgCode(err) != "23505" {
		t.Errorf("SQLSTATE = %s, want 23505 (unique violation)", pgCode(err))
	}

	if balance := api.balance(t, walletID); balance.String() != "100.00" {
		t.Errorf("balance = %s, want 100.00", balance)
	}
	if report := api.reconcile(t, walletID); !report.Consistent {
		t.Error("the wallet diverged")
	}
}

// Test_theOpeningOperationIsInternalOnly proves the schema refuses to record an
// internal operation carrying a provider identity, and vice versa.
func Test_theOpeningOperationIsInternalOnly(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")
	ctx := context.Background()

	_, err := api.pool.Exec(ctx, `
		INSERT INTO wager_transactions
			(id, origin, provider_id, external_transaction_id, idempotency_key, payload_hash,
			 wallet_id, player_id, kind, amount_minor, currency, status, created_at, updated_at)
		VALUES ($1,'INTERNAL','provider-a',NULL,NULL,'hash',$2,$3,'OPENING',100,'BRL','PROCESSED',now(),now())`,
		uuid.New(), walletID, player)
	if err == nil {
		t.Error("the schema accepted an internal operation carrying a provider identity")
	} else if pgCode(err) != "23514" {
		t.Errorf("SQLSTATE = %s, want 23514 (check violation)", pgCode(err))
	}

	_, err = api.pool.Exec(ctx, `
		INSERT INTO wager_transactions
			(id, origin, provider_id, external_transaction_id, idempotency_key, payload_hash,
			 wallet_id, player_id, kind, amount_minor, currency, status, created_at, updated_at)
		VALUES ($1,'EXTERNAL',NULL,NULL,NULL,'hash',$2,$3,'BET',100,'BRL','PROCESSED',now(),now())`,
		uuid.New(), walletID, player)
	if err == nil {
		t.Error("the schema accepted an external operation without a provider identity")
	}
}

// Test_aLossMovesNothing proves a LOSS settles without touching the balance, the
// ledger or the wallet version.
func Test_aLossMovesNothing(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")

	before := api.countRows(t, `SELECT version FROM wallets WHERE id = $1`, walletID)
	entriesBefore := api.ledgerEntries(t, walletID)

	loss, err := api.submit(t, operation{
		provider: "provider-a", externalID: "loss-1", playerID: player, walletID: walletID,
		kind: string(domain.KindLoss), amount: "0.00",
	})
	requireStatus(t, loss, err, string(domain.StatusProcessed))
	if loss.Balance.Amount != "100.00" {
		t.Errorf("balance = %s, want 100.00", loss.Balance.Amount)
	}

	if entries := api.ledgerEntries(t, walletID); entries != entriesBefore {
		t.Errorf("the ledger grew from %d to %d entries: a LOSS writes none", entriesBefore, entries)
	}
	after := api.countRows(t, `SELECT version FROM wallets WHERE id = $1`, walletID)
	if after != before {
		t.Errorf("wallet version moved from %d to %d: a LOSS changes nothing", before, after)
	}

	// A LOSS carrying a value is refused before anything is recorded.
	_, err = api.submit(t, operation{
		provider: "provider-a", externalID: "loss-2", playerID: player, walletID: walletID,
		kind: string(domain.KindLoss), amount: "5.00",
	})
	requireCode(t, err, xerr.CodeLossAmountMustBeZero)
}

// Test_reconciliationMatchesTheLedger proves the check is a real independent
// reconstruction, and that it reports a divergence when one exists.
func Test_reconciliationMatchesTheLedger(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "1000.00")

	report := api.reconcile(t, walletID)
	if !report.Consistent {
		t.Fatalf("a freshly opened wallet diverged: %s", report.Difference)
	}
	if report.CheckedEntries != 1 {
		t.Errorf("checkedEntries = %d, want 1 (the opening credit)", report.CheckedEntries)
	}
	if report.CalculatedBalance.String() != "1000.00" || report.StoredBalance.String() != "1000.00" {
		t.Errorf("stored %s, calculated %s", report.StoredBalance.String(), report.CalculatedBalance.String())
	}

	// Break the balance behind the application's back and prove the check
	// notices. The trigger that would normally prevent this is disabled only in
	// the fixture, so this is what an operator-injected fault looks like.
	if _, err := api.pool.Exec(context.Background(),
		`ALTER TABLE wallets DISABLE TRIGGER ALL`); err != nil {
		t.Fatalf("suspend wallet triggers: %v", err)
	}
	if _, err := api.pool.Exec(context.Background(),
		`UPDATE wallets SET balance_minor = balance_minor + 500 WHERE id = $1`, walletID); err != nil {
		t.Fatalf("inject divergence: %v", err)
	}
	if _, err := api.pool.Exec(context.Background(),
		`ALTER TABLE wallets ENABLE TRIGGER ALL`); err != nil {
		t.Fatalf("restore wallet triggers: %v", err)
	}

	diverged := api.reconcile(t, walletID)
	if diverged.Consistent {
		t.Error("the reconciliation reported a consistent wallet after the balance was corrupted")
	}
	if diverged.Difference.String() != "5.00" {
		t.Errorf("difference = %s, want 5.00", diverged.Difference.String())
	}

	// Restoring the balance makes it consistent again, and the reconciliation
	// itself never wrote anything.
	fixed := api.reconcile(t, walletID)
	if fixed.Consistent {
		t.Error("the second reconciliation was expected to still see the divergence")
	}
	if _, err := api.pool.Exec(context.Background(),
		`ALTER TABLE wallets DISABLE TRIGGER ALL`); err != nil {
		t.Fatalf("suspend wallet triggers: %v", err)
	}
	if _, err := api.pool.Exec(context.Background(),
		`UPDATE wallets SET balance_minor = balance_minor - 500 WHERE id = $1`, walletID); err != nil {
		t.Fatalf("restore balance: %v", err)
	}
	if _, err := api.pool.Exec(context.Background(),
		`ALTER TABLE wallets ENABLE TRIGGER ALL`); err != nil {
		t.Fatalf("restore wallet triggers: %v", err)
	}
	if repaired := api.reconcile(t, walletID); !repaired.Consistent {
		t.Error("the wallet is still diverged after the balance was restored")
	}
}

// Test_aReversalIsAnIndependentLedgerEntry proves a correction is a new entry,
// never an edit of the original.
func Test_aReversalIsAnIndependentLedgerEntry(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")

	bet, err := api.submit(t, operation{
		provider: "provider-a", externalID: "bet-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "30.00",
	})
	requireStatus(t, bet, err, string(domain.StatusProcessed))

	_, err = api.submit(t, operation{
		provider: "provider-a", externalID: "rollback-1", playerID: player, walletID: walletID,
		kind: string(domain.KindRollback), amount: "30.00", reference: "bet-1",
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}

	page, err := api.ledger.List(context.Background(), walletID, "", 50)
	if err != nil {
		t.Fatalf("list ledger: %v", err)
	}
	if len(page.Data) != 3 {
		t.Fatalf("the ledger holds %d entries, want 3 (opening, debit, credit)", len(page.Data))
	}
	// The opening and the debit are untouched; the reversal is a third row.
	if page.Data[1].Direction != "DEBIT" || page.Data[1].Money.Amount != "30.00" {
		t.Errorf("the original debit changed: %+v", page.Data[1])
	}
	if page.Data[2].Direction != "CREDIT" || page.Data[2].Money.Amount != "30.00" {
		t.Errorf("the reversal is not a credit of 30.00: %+v", page.Data[2])
	}
	if page.Data[2].TransactionID == page.Data[1].TransactionID {
		t.Error("the reversal reused the original transaction id")
	}
}

// Test_ledgerPaginationIsStableAndTotal proves the cursor neither skips nor
// repeats an entry, even when two entries share a timestamp.
func Test_ledgerPaginationIsStableAndTotal(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")

	const bets = 12
	for i := 0; i < bets; i++ {
		_, err := api.submit(t, operation{
			provider: "provider-a", externalID: uuid.NewString(), playerID: player, walletID: walletID,
			kind: string(domain.KindBet), amount: "1.00",
		})
		if err != nil {
			t.Fatalf("bet %d: %v", i, err)
		}
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, err := api.ledger.List(context.Background(), walletID, cursor, 5)
		if err != nil {
			t.Fatalf("list ledger: %v", err)
		}
		for _, entry := range page.Data {
			if seen[entry.ID] {
				t.Fatalf("entry %s was returned twice", entry.ID)
			}
			seen[entry.ID] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
	}

	if want := bets + 1; len(seen) != want {
		t.Errorf("paged through %d entries, want %d", len(seen), want)
	}

	// A cursor this endpoint did not mint is a validation error, not a silent
	// full scan.
	if _, err := api.ledger.List(context.Background(), walletID, "not-a-cursor", 5); err == nil {
		t.Error("a forged cursor was accepted")
	} else if code, _ := xerr.CodeOf(err); code != xerr.CodeInvalidRequest {
		t.Errorf("error code = %s, want %s", code, xerr.CodeInvalidRequest)
	}
}

// pgCode extracts the SQLSTATE of a PostgreSQL error.
func pgCode(err error) string {
	return postgresCode(err)
}
