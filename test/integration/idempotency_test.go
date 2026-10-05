//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/app/usecase"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// Test_replayRepeatsTheOriginalResult proves a repeated delivery returns the
// stored outcome — including the balance observed the first time — even after
// the wallet has moved on.
func Test_replayRepeatsTheOriginalResult(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")

	first, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "25.00",
	})
	requireStatus(t, first, err, string(domain.StatusProcessed))
	if first.Balance.Amount != "75.00" {
		t.Fatalf("first balance = %s, want 75.00", first.Balance.Amount)
	}
	if first.IdempotentReplay {
		t.Error("the first delivery was reported as a replay")
	}

	// The wallet moves on, so a replay that re-read the current balance would
	// report something the provider never saw.
	second, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-2", playerID: player, walletID: walletID,
		kind: string(domain.KindWin), amount: "10.00",
	})
	requireStatus(t, second, err, string(domain.StatusProcessed))
	if api.balance(t, walletID).String() != "85.00" {
		t.Fatalf("balance after the win = %s, want 85.00", api.balance(t, walletID))
	}

	replay, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "25.00",
	})
	requireStatus(t, replay, err, string(domain.StatusProcessed))
	if !replay.IdempotentReplay {
		t.Error("the repeated delivery was not recognised as a replay")
	}
	if replay.TransactionID != first.TransactionID {
		t.Errorf("replay transactionId = %s, want the original %s", replay.TransactionID, first.TransactionID)
	}
	if replay.Balance.Amount != "75.00" {
		t.Errorf("replay balance = %s, want the originally observed 75.00", replay.Balance.Amount)
	}
	if debits := api.countDebits(t, walletID); debits != 1 {
		t.Errorf("ledger holds %d debits, want 1: a replay moves nothing", debits)
	}
}

// Test_reusedKeyWithADifferentPayloadIsAConflict proves the fingerprint covers
// the business payload.
func Test_reusedKeyWithADifferentPayloadIsAConflict(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")

	_, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "25.00",
	})
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	_, err = api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "26.00",
	})
	requireCode(t, err, xerr.CodeIdempotencyKeyConflict)
	if status := httpStatus(t, err); status != 409 {
		t.Errorf("status = %d, want 409", status)
	}
}

// Test_equivalentAmountsAreTheSameOperation proves the normalisation happens
// before the hash: "25", "25.0" and "25.00" are one request.
func Test_equivalentAmountsAreTheSameOperation(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")

	first, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "25",
	})
	requireStatus(t, first, err, string(domain.StatusProcessed))

	for _, amount := range []string{"25.0", "25.00"} {
		replay, err := api.submit(t, operation{
			provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
			kind: string(domain.KindBet), amount: amount,
		})
		requireStatus(t, replay, err, string(domain.StatusProcessed))
		if !replay.IdempotentReplay {
			t.Errorf("amount %q was not recognised as the same operation", amount)
		}
	}

	if debits := api.countDebits(t, walletID); debits != 1 {
		t.Errorf("ledger holds %d debits, want 1", debits)
	}
}

// Test_theSameOperationUnderAnotherKeyIsAConflict proves the provider's own
// identity of an operation is unique: an operation cannot be applied twice
// under two different idempotency keys.
func Test_theSameOperationUnderAnotherKeyIsAConflict(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")

	_, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "25.00",
	})
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	// A different idempotency key, the same external transaction.
	req := usecase.OperationRequest{
		IdempotencyKey:        "provider-a:" + scopeOf(t).id("some-other-key"),
		ProviderID:            "provider-a",
		ExternalTransactionID: scopeOf(t).id("t-1"),
		PlayerID:              player,
		WalletID:              walletID,
		RoundID:               "round-1",
		GameID:                "fortune-chimp",
		Kind:                  string(domain.KindBet),
		Amount:                "25.00",
		Currency:              "BRL",
		Source:                usecase.SourceHTTP,
	}
	_, err = api.wager.Submit(context.Background(), req)
	requireCode(t, err, xerr.CodeExternalTransactionConflict)

	if debits := api.countDebits(t, walletID); debits != 1 {
		t.Errorf("ledger holds %d debits, want 1", debits)
	}
}

// Test_httpAndSqsShareOneOperation is the cross-transport guarantee: the same
// operation delivered over HTTP and then over the broker is one operation.
func Test_httpAndSqsShareOneOperation(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")

	overHTTP, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "30.00", source: usecase.SourceHTTP,
	})
	requireStatus(t, overHTTP, err, string(domain.StatusProcessed))

	overSQS, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "30.00", source: usecase.SourceSQS,
		inbox: &usecase.InboxContext{
			ConsumerName: "wager-operations",
			MessageID:    "msg-1",
			PayloadHash:  "hash-1",
			ReceivedAt:   nowUTC(),
		},
	})
	requireStatus(t, overSQS, err, string(domain.StatusProcessed))
	if !overSQS.IdempotentReplay {
		t.Error("the broker delivery was not recognised as the same operation")
	}
	if overSQS.TransactionID != overHTTP.TransactionID {
		t.Errorf("broker transactionId = %s, want the HTTP one %s", overSQS.TransactionID, overHTTP.TransactionID)
	}

	// And the other way round: the broker first, HTTP second.
	player2 := uuid.New()
	wallet2 := api.openWallet(t, player2, "100.00")
	first, err := api.submit(t, operation{
		provider: "provider-b", externalID: "t-9", playerID: player2, walletID: wallet2,
		kind: string(domain.KindBet), amount: "15.00", source: usecase.SourceSQS,
		inbox: &usecase.InboxContext{
			ConsumerName: "wager-operations", MessageID: "msg-9", PayloadHash: "hash-9", ReceivedAt: nowUTC(),
		},
	})
	requireStatus(t, first, err, string(domain.StatusProcessed))
	second, err := api.submit(t, operation{
		provider: "provider-b", externalID: "t-9", playerID: player2, walletID: wallet2,
		kind: string(domain.KindBet), amount: "15.00", source: usecase.SourceHTTP,
	})
	requireStatus(t, second, err, string(domain.StatusProcessed))
	if !second.IdempotentReplay {
		t.Error("the HTTP delivery was not recognised as the broker's operation")
	}

	for _, walletID := range []uuid.UUID{walletID, wallet2} {
		if debits := api.countDebits(t, walletID); debits != 1 {
			t.Errorf("wallet %s holds %d debits, want 1", walletID, debits)
		}
	}
}

// Test_theInboxAbsorbsARedeliveredMessage proves at-least-once delivery becomes
// at-most-once processing: the same message identity, delivered again after the
// work committed, replays the stored result instead of moving money.
func Test_theInboxAbsorbsARedeliveredMessage(t *testing.T) {
	api := newInstance(t, "api")

	player := uuid.New()
	walletID := api.openWallet(t, player, "100.00")
	inboxCtx := &usecase.InboxContext{
		ConsumerName: "wager-operations",
		MessageID:    "msg-redelivery",
		PayloadHash:  "hash-redelivery",
		ReceivedAt:   nowUTC(),
	}

	first, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "40.00",
		source: usecase.SourceSQS, inbox: inboxCtx,
	})
	requireStatus(t, first, err, string(domain.StatusProcessed))
	if first.IdempotentReplay {
		t.Error("the first delivery was reported as a replay")
	}

	// The consumer crashed after committing but before deleting the message, so
	// the broker hands it over again.
	second, err := api.submit(t, operation{
		provider: "provider-a", externalID: "t-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "40.00",
		source: usecase.SourceSQS, inbox: inboxCtx,
	})
	requireStatus(t, second, err, string(domain.StatusProcessed))
	if !second.IdempotentReplay {
		t.Error("the redelivered message was processed again")
	}
	if second.TransactionID != first.TransactionID {
		t.Errorf("redelivery produced a new transaction %s", second.TransactionID)
	}

	if balance := api.balance(t, walletID); balance.String() != "60.00" {
		t.Errorf("balance = %s, want 60.00", balance)
	}
	if debits := api.countDebits(t, walletID); debits != 1 {
		t.Errorf("ledger holds %d debits, want 1", debits)
	}
	if report := api.reconcile(t, walletID); !report.Consistent {
		t.Error("the wallet diverged after a redelivery")
	}

	// The inbox recorded the message exactly once, and marked it complete.
	claimed := api.countRows(t,
		`SELECT count(*) FROM inbox_messages WHERE consumer_name = 'wager-operations' AND message_id = $1`,
		"msg-redelivery")
	if claimed != 1 {
		t.Errorf("inbox holds %d rows for the message, want 1", claimed)
	}
	completed := api.countRows(t,
		`SELECT count(*) FROM inbox_messages WHERE message_id = $1 AND completed_at IS NOT NULL`, "msg-redelivery")
	if completed != 1 {
		t.Errorf("the message was not marked complete")
	}
}

// Test_aPendingReferenceSurvivesAndResolves covers a reversal that arrives
// before the operation it reverses, and is resolved by another instance.
func Test_aPendingReferenceSurvivesAndResolves(t *testing.T) {
	receiver := newInstance(t, "receiver")
	resolver := newInstance(t, "resolver")

	player := uuid.New()
	walletID := receiver.openWallet(t, player, "100.00")

	// The reversal arrives first, as providers deliver independently.
	pending, err := receiver.submit(t, operation{
		provider: "provider-a", externalID: "refund-1", playerID: player, walletID: walletID,
		kind: string(domain.KindRefund), amount: "30.00", reference: "bet-1",
	})
	requireStatus(t, pending, err, string(domain.StatusPendingReference))
	if httpStatusOf(pending) != 202 {
		t.Errorf("a pending reference should be reported as 202, got %s", pending.Status)
	}
	if apiBalance := receiver.balance(t, walletID); apiBalance.String() != "100.00" {
		t.Errorf("the wallet moved to %s while waiting for a reference", apiBalance)
	}

	// A different instance picks the pending reversal up and retries it before
	// the reference has arrived: it stays pending and the attempt is recorded.
	due := resolverDueAfter(t, receiver, testConfig.Wagering.ReferenceBackoffBase, pending.TransactionID)
	if len(due) != 1 {
		t.Fatalf("the resolver found %d pending references, want 1", len(due))
	}
	resolver.resume(t, due[0])
	if status := resolverStatus(t, receiver, pending.TransactionID); status != string(domain.StatusPendingReference) {
		t.Errorf("status after a retry with no reference = %s, want PENDING_REFERENCE", status)
	}

	// The bet finally arrives.
	bet, err := receiver.submit(t, operation{
		provider: "provider-a", externalID: "bet-1", playerID: player, walletID: walletID,
		kind: string(domain.KindBet), amount: "30.00",
	})
	requireStatus(t, bet, err, string(domain.StatusProcessed))

	// The next sweep resolves the waiting refund.
	time.Sleep(testConfig.Wagering.ReferenceBackoffBase + 20*time.Millisecond)
	resolved := resolverSweep(t, resolver, receiver, pending.TransactionID)
	if resolved != string(domain.StatusProcessed) {
		t.Fatalf("the pending reversal settled as %s, want PROCESSED", resolved)
	}
	if balance := receiver.balance(t, walletID); balance.String() != "100.00" {
		t.Errorf("balance = %s, want 100.00 after a 30.00 bet refunded in full", balance)
	}
	if report := receiver.reconcile(t, walletID); !report.Consistent {
		t.Error("the wallet diverged after a resolved reversal")
	}
}

// Test_aPendingReferenceIsRejectedWhenItNeverArrives covers the TTL: a reversal
// naming a reference that never shows up is rejected with a stable code, not
// left pending forever.
func Test_aPendingReferenceIsRejectedWhenItNeverArrives(t *testing.T) {
	receiver := newInstance(t, "receiver")
	resolver := newInstance(t, "resolver")

	player := uuid.New()
	walletID := receiver.openWallet(t, player, "100.00")

	pending, err := receiver.submit(t, operation{
		provider: "provider-a", externalID: "rollback-1", playerID: player, walletID: walletID,
		kind: string(domain.KindRollback), amount: "30.00", reference: "never-arrives",
	})
	requireStatus(t, pending, err, string(domain.StatusPendingReference))

	// Sweep until the retry budget is spent.
	// Each retry is scheduled with a backoff that lives in the database, so a
	// sweep only finds the operation once its next attempt is due — the same
	// thing the worker's ticker sees.
	var last string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(testConfig.Wagering.ReferenceBackoffBase + 20*time.Millisecond)
		due := resolverDue(t, receiver, pending.TransactionID)
		if len(due) == 0 {
			continue
		}
		resolver.resume(t, due[0])
		last = resolverStatus(t, receiver, pending.TransactionID)
		if last == string(domain.StatusRejected) {
			break
		}
	}

	if last != string(domain.StatusRejected) {
		t.Fatalf("the reversal ended as %s, want REJECTED once the budget is spent", last)
	}
	view, err := receiver.queries.GetTransaction(context.Background(), pending.TransactionID)
	if err != nil {
		t.Fatalf("read transaction: %v", err)
	}
	if view.FailureCode != string(xerr.CodeReferenceNotFound) {
		t.Errorf("failureCode = %s, want %s", view.FailureCode, xerr.CodeReferenceNotFound)
	}
	if balance := receiver.balance(t, walletID); balance.String() != "100.00" {
		t.Errorf("balance = %s, want 100.00: an unresolvable reversal moves nothing", balance)
	}
}
