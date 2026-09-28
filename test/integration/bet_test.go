package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/ironledger/ironledger/internal/usecase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are the ones that can only be written against a real database:
// they prove the transaction, the row lock and the constraints actually hold,
// rather than that the code calls the right methods in the right order.

func decode(t *testing.T, out usecase.ApplyOutput) usecase.OperationResult {
	t.Helper()
	var res usecase.OperationResult
	require.NoError(t, json.Unmarshal(out.Body, &res))
	return res
}

// T033: a valid bet debits the balance and leaves one ledger entry, one
// idempotency record, one outbox row and one audit row.
func TestT033_ValidBetCommits(t *testing.T) {
	walletID := newWallet(t, 100_00, domain.BRL, domain.WalletActive)

	out, err := applyBet(t, walletID, "k1", 30_00, domain.BRL)
	require.NoError(t, err)
	assert.False(t, out.Replay)

	res := decode(t, out)
	assert.Equal(t, int64(70_00), res.BalanceMinor)
	assert.Equal(t, domain.BRL, res.Currency)
	assert.Len(t, res.LedgerEntryIDs, 1)

	assert.Equal(t, int64(70_00), balanceOf(t, walletID))
	assert.Equal(t, 1, countRows(t, `SELECT count(*)::int FROM ledger_entries WHERE wallet_id = $1`, walletID))
	assert.Equal(t, 1, countRows(t, `SELECT count(*)::int FROM operations WHERE wallet_id = $1`, walletID))
	assert.Equal(t, 1, countRows(t,
		`SELECT count(*)::int FROM outbox o JOIN bets b ON b.id = o.aggregate_id
		 WHERE b.wallet_id = $1 AND o.event_type = 'wallet.bet_placed'`, walletID))
	assert.Equal(t, 1, countRows(t,
		`SELECT count(*)::int FROM audit_log WHERE wallet_id = $1 AND outcome = 'ACCEPTED'`, walletID))

	// The stored sequence must match the response's balance_after, so a later
	// reconciliation can trust the ledger without recomputing it.
	assert.Equal(t, 1, countRows(t, `
		SELECT count(*)::int FROM ledger_entries
		WHERE wallet_id = $1 AND sequence = 1 AND balance_after_minor = 7000 AND amount_minor = 3000`,
		walletID))
}

// T034: the same key with the same body replays the stored response byte for
// byte and changes nothing.
func TestT034_RepeatRequestReplays(t *testing.T) {
	walletID := newWallet(t, 100_00, domain.BRL, domain.WalletActive)

	first, err := applyBet(t, walletID, "same-key", 30_00, domain.BRL)
	require.NoError(t, err)

	// Three retries, as a flaky caller would actually produce.
	for range 3 {
		again, err := applyBet(t, walletID, "same-key", 30_00, domain.BRL)
		require.NoError(t, err)
		assert.True(t, again.Replay)
		assert.Equal(t, string(first.Body), string(again.Body),
			"a replay must return the stored original, not a recomputation")
	}

	assert.Equal(t, int64(70_00), balanceOf(t, walletID), "a replay must not move the balance")
	assert.Equal(t, 1, countRows(t, `SELECT count(*)::int FROM ledger_entries WHERE wallet_id = $1`, walletID))
	assert.Equal(t, 1, countRows(t, `SELECT count(*)::int FROM operations WHERE wallet_id = $1`, walletID))
	assert.Equal(t, 1, countRows(t,
		`SELECT count(*)::int FROM outbox o JOIN bets b ON b.id = o.aggregate_id
		 WHERE b.wallet_id = $1 AND o.event_type = 'wallet.bet_placed'`, walletID))
}

// A rejected request writes no operations row, so a legitimate retry after the
// caller fixes the problem is never blocked by the earlier failure.
func TestRejectionLeavesNoIdempotencyRecord(t *testing.T) {
	walletID := newWallet(t, 10_00, domain.BRL, domain.WalletActive)

	_, err := applyBet(t, walletID, "retry-me", 30_00, domain.BRL)
	require.Error(t, err)
	assert.Equal(t, domain.ReasonInsufficientFunds, domain.CodeOf(err))
	assert.Equal(t, 0, countRows(t, `SELECT count(*)::int FROM operations WHERE wallet_id = $1`, walletID),
		"a rejection must not create an idempotency record")
	assert.Equal(t, 0, countRows(t, `SELECT count(*)::int FROM ledger_entries WHERE wallet_id = $1`, walletID))
	assert.Equal(t, 1, countRows(t,
		`SELECT count(*)::int FROM audit_log WHERE wallet_id = $1 AND reason_code = 'INSUFFICIENT_FUNDS'`, walletID))

	// Same key, but now affordable: it must be accepted, not treated as a
	// replay of the refusal. The top-up lands on 100.00, so the bet leaves 70.00.
	_, err = sharedPool.Exec(context.Background(),
		`UPDATE wallets SET balance_minor = 10000 WHERE id = $1`, walletID)
	require.NoError(t, err)

	out, err := applyBet(t, walletID, "retry-me", 30_00, domain.BRL)
	require.NoError(t, err)
	assert.False(t, out.Replay)
	assert.Equal(t, int64(70_00), balanceOf(t, walletID))
}

// T036: an insufficient balance is refused by the domain and the database, and
// the refusal is recorded.
func TestT036_InsufficientFundsIsRefused(t *testing.T) {
	walletID := newWallet(t, 10_00, domain.BRL, domain.WalletActive)

	_, err := applyBet(t, walletID, "too-much", 30_00, domain.BRL)
	require.Error(t, err)
	assert.Equal(t, domain.ReasonInsufficientFunds, domain.CodeOf(err))

	var domErr *domain.Error
	require.ErrorAs(t, err, &domErr)
	require.NotNil(t, domErr.AvailableMinor, "the caller needs the available balance to size a retry")
	assert.Equal(t, int64(10_00), *domErr.AvailableMinor)

	assert.Equal(t, int64(10_00), balanceOf(t, walletID))
	assert.Equal(t, 0, countRows(t, `SELECT count(*)::int FROM ledger_entries WHERE wallet_id = $1`, walletID))
}

// The database, not only the domain, refuses a negative balance. A caller that
// somehow bypassed the domain still cannot corrupt the balance.
func TestDatabaseRefusesNegativeBalance(t *testing.T) {
	walletID := newWallet(t, 10_00, domain.BRL, domain.WalletActive)

	_, err := sharedPool.Exec(context.Background(),
		`UPDATE wallets SET balance_minor = balance_minor - 100000 WHERE id = $1`, walletID)
	require.Error(t, err)
	assert.Equal(t, "23514", postgres_PgErrorCode(err), "expected a check-constraint violation")

	assert.Equal(t, int64(10_00), balanceOf(t, walletID))
}

// The ledger is append-only at the database level: not even the owner of the
// connection can edit history (Constitution Principle II).
func TestLedgerEntriesAreAppendOnly(t *testing.T) {
	walletID := newWallet(t, 100_00, domain.BRL, domain.WalletActive)
	_, err := applyBet(t, walletID, "k1", 30_00, domain.BRL)
	require.NoError(t, err)

	_, err = sharedPool.Exec(context.Background(),
		`UPDATE ledger_entries SET amount_minor = 1 WHERE wallet_id = $1`, walletID)
	require.Error(t, err)
	assert.Equal(t, "23001", postgres_PgErrorCode(err))

	_, err = sharedPool.Exec(context.Background(),
		`DELETE FROM ledger_entries WHERE wallet_id = $1`, walletID)
	require.Error(t, err)
	assert.Equal(t, "23001", postgres_PgErrorCode(err))

	assert.Equal(t, 1, countRows(t, `SELECT count(*)::int FROM ledger_entries WHERE wallet_id = $1`, walletID))
}

// Concurrent requests for different wallets must not block each other: the lock
// is per wallet row, not global.
func TestConcurrentRequestsForDistinctWalletsAllSucceed(t *testing.T) {
	const wallets = 8
	ids := make([]string, wallets)
	for i := range ids {
		ids[i] = newWallet(t, 100_00, domain.BRL, domain.WalletActive)
	}

	var wg sync.WaitGroup
	errs := make([]error, wallets)
	start := make(chan struct{})
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = applyBet(t, ids[i], fmt.Sprintf("k%d", i), 50_00, domain.BRL)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "wallet %d", i)
		assert.Equal(t, int64(50_00), balanceOf(t, ids[i]))
	}
}

// The concurrency test that matters: many goroutines race on the SAME wallet with
// DIFFERENT keys. The row lock must serialize them, every balance must be
// correct, and no entry may be lost or double-counted.
func TestConcurrentBetsOnSameWalletNeverLoseOrDoubleCount(t *testing.T) {
	const (
		parallel = 12
		each     = 10
		stake    = 1_00
	)
	walletID := newWallet(t, int64(parallel*each)*stake, domain.BRL, domain.WalletActive)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for p := range parallel {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			<-start
			for i := range each {
				key := fmt.Sprintf("w%d-o%d", p, i)
				out, err := applyBet(t, walletID, key, stake, domain.BRL)
				if err != nil {
					t.Errorf("apply %s: %v", key, err)
					return
				}
				if out.Replay {
					t.Errorf("apply %s: a fresh key must not replay", key)
					return
				}
			}
		}(p)
	}
	close(start)
	wg.Wait()

	total := parallel * each
	assert.Equal(t, int64(0), balanceOf(t, walletID),
		"every debit must be reflected exactly once")
	assert.Equal(t, total, countRows(t, `SELECT count(*)::int FROM ledger_entries WHERE wallet_id = $1`, walletID))
	assert.Equal(t, total, countRows(t, `SELECT count(*)::int FROM operations WHERE wallet_id = $1`, walletID))
	assert.Equal(t, total, countRows(t,
		`SELECT count(*)::int FROM outbox WHERE aggregate_id IN (SELECT id FROM bets WHERE wallet_id = $1)`, walletID))

	// Sequences must be a gapless per-wallet run, and each entry's recorded
	// balance must equal the running total: this is what makes the ledger
	// auditable without replaying the wallet table.
	assert.Equal(t, 0, countRows(t, `
		SELECT count(*)::int FROM (
			SELECT sequence, balance_after_minor, amount_minor,
			       row_number() OVER (ORDER BY sequence) AS rn
			FROM ledger_entries WHERE wallet_id = $1
		) t
		WHERE sequence <> rn OR balance_after_minor <> 12000 - (rn * 100)`,
		walletID), "sequence must be gapless and balance_after must be the running total")
}

// The same key raced from many goroutines: exactly one must apply, and every
// caller must observe the same stored response.
func TestConcurrentSameKeyAppliesExactlyOnce(t *testing.T) {
	const goroutines = 12
	walletID := newWallet(t, 100_00, domain.BRL, domain.WalletActive)

	type result struct {
		body   []byte
		replay bool
		err    error
	}
	results := make([]result, goroutines)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out, err := applyBet(t, walletID, "one-key", 30_00, domain.BRL)
			results[i] = result{body: out.Body, replay: out.Replay, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	applied := 0
	for i, r := range results {
		require.NoError(t, r.err, "goroutine %d: a duplicate of an accepted request must not error", i)
		assert.Equal(t, string(results[0].body), string(r.body),
			"every caller must see the same stored response, goroutine %d", i)
		if !r.replay {
			applied++
		}
	}
	assert.Equal(t, 1, applied, "exactly one goroutine may be the original")
	assert.Equal(t, int64(70_00), balanceOf(t, walletID))
	assert.Equal(t, 1, countRows(t, `SELECT count(*)::int FROM ledger_entries WHERE wallet_id = $1`, walletID))
}

// A frozen wallet is refused, and so is a currency the wallet does not hold.
func TestFrozenWalletAndCurrencyMismatchAreRefused(t *testing.T) {
	frozen := newWallet(t, 100_00, domain.BRL, domain.WalletFrozen)
	_, err := applyBet(t, frozen, "k1", 10_00, domain.BRL)
	require.Error(t, err)
	assert.Equal(t, domain.ReasonWalletFrozen, domain.CodeOf(err))

	active := newWallet(t, 100_00, domain.BRL, domain.WalletActive)
	_, err = applyBet(t, active, "k1", 10_00, domain.USD)
	require.Error(t, err)
	assert.Equal(t, domain.ReasonCurrencyMismatch, domain.CodeOf(err))

	assert.Equal(t, int64(100_00), balanceOf(t, active))
	assert.Equal(t, 0, countRows(t, `SELECT count(*)::int FROM ledger_entries WHERE wallet_id = $1`, active))
}

// A wallet that does not exist is a 404, not a 500, and it leaves no trace.
func TestUnknownWalletIsRefused(t *testing.T) {
	_, err := applyBet(t, "00000000-0000-4000-8000-000000000000", "k1", 10_00, domain.BRL)
	require.Error(t, err)
	assert.Equal(t, domain.ReasonWalletNotFound, domain.CodeOf(err))
}

// A token tenant may not touch another tenant's wallet.
func TestTenantMismatchIsRefused(t *testing.T) {
	walletID := newWallet(t, 100_00, domain.BRL, domain.WalletActive)

	_, err := sharedSvc.Apply(context.Background(), usecase.ApplyInput{
		WalletID:       walletID,
		TenantID:       "99999999-9999-4999-8999-999999999999",
		IdempotencyKey: "k1",
		Operation: domain.Operation{
			Type:          domain.OperationBET,
			Amount:        domain.Money{AmountMinor: 10_00, Currency: domain.BRL},
			TransactionID: "txn-k1",
		},
		Channel: domain.ChannelAPI,
		Actor:   testActor,
	})
	require.Error(t, err)
	assert.Equal(t, domain.ReasonTenantMismatch, domain.CodeOf(err))
	assert.Equal(t, int64(100_00), balanceOf(t, walletID))
}

// T046: the balance read returns the exact stored integer.
func TestT046_BalanceRead(t *testing.T) {
	walletID := newWallet(t, 123_456_789, domain.BRL, domain.WalletActive)
	_, err := applyBet(t, walletID, "k1", 1, domain.BRL)
	require.NoError(t, err)

	view, err := sharedSvc.GetBalance(context.Background(), testTenantID, walletID)
	require.NoError(t, err)
	assert.Equal(t, int64(123_456_788), view.BalanceMinor)
	assert.Equal(t, domain.BRL, view.Currency)
	assert.Equal(t, walletID, view.WalletID)
}

// The outbox row is in the same transaction as the effect, so a rolled-back
// operation must not leave a publishable event behind.
func TestFailedOperationLeavesNoOutboxRow(t *testing.T) {
	walletID := newWallet(t, 10_00, domain.BRL, domain.WalletActive)

	_, err := applyBet(t, walletID, "k1", 30_00, domain.BRL)
	require.Error(t, err)

	assert.Equal(t, 0, countRows(t,
		`SELECT count(*)::int FROM outbox o JOIN bets b ON b.id = o.aggregate_id WHERE b.wallet_id = $1`, walletID),
		"a rolled-back operation must leave no event to publish")
}
