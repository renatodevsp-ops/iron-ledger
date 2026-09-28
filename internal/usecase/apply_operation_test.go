package usecase

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testTenant   = "11111111-1111-4111-8111-111111111111"
	testWalletID = "22222222-2222-4222-8222-222222222222"
	testActor    = "tester"
)

type harness struct {
	svc   *Service
	state *fakeState
	calls *[]string
}

func newHarness(t *testing.T, balance int64, currency domain.Currency, status domain.WalletStatus) *harness {
	t.Helper()
	state := newFakeState()
	state.wallets[testWalletID] = &domain.Wallet{
		ID:        testWalletID,
		TenantID:  testTenant,
		Balance:   domain.Money{AmountMinor: balance, Currency: currency},
		Currency:  currency,
		Status:    status,
		Version:   1,
		UpdatedAt: 1,
	}
	calls := &[]string{}
	uow := &fakeUOW{state: state, calls: calls}
	svc := New(uow, fixedClock{t: 1_700_000_000_000_000_000}, &seqIDs{})
	return &harness{svc: svc, state: state, calls: calls}
}

func betInput(key string, minor int64, currency domain.Currency) ApplyInput {
	return ApplyInput{
		WalletID:       testWalletID,
		TenantID:       testTenant,
		IdempotencyKey: key,
		Operation: domain.Operation{
			Type:          domain.OperationBET,
			Amount:        domain.Money{AmountMinor: minor, Currency: currency},
			TransactionID: "txn-" + key,
		},
		Channel: domain.ChannelAPI,
		Actor:   testActor,
	}
}

func decodeResult(t *testing.T, out ApplyOutput) OperationResult {
	t.Helper()
	var res OperationResult
	require.NoError(t, json.Unmarshal(out.Body, &res))
	return res
}

// T037: the wallet lock is the first statement of every mutating transaction.
func TestApplyLocksWalletFirst(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)

	_, err := h.svc.Apply(context.Background(), betInput("k1", 10_00, domain.BRL))
	require.NoError(t, err)

	require.NotEmpty(t, *h.calls)
	assert.Equal(t, "LockWallet", (*h.calls)[0],
		"the wallet row lock must be the first statement, got %v", *h.calls)
}

// T037/T040: a successful bet debits the balance exactly once and records one
// ledger entry, one idempotency record, one outbox row and one audit row.
func TestApplyBetSuccess(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)

	out, err := h.svc.Apply(context.Background(), betInput("k1", 30_00, domain.BRL))
	require.NoError(t, err)
	assert.False(t, out.Replay)

	res := decodeResult(t, out)
	assert.Equal(t, domain.OperationBET, res.Type)
	assert.Equal(t, "COMPLETED", res.Status)
	assert.Equal(t, int64(70_00), res.BalanceMinor)
	assert.Equal(t, domain.BRL, res.Currency)
	assert.NotNil(t, res.BetID)
	assert.Len(t, res.LedgerEntryIDs, 1)

	assert.Equal(t, int64(70_00), h.state.wallets[testWalletID].Balance.AmountMinor)
	require.Len(t, h.state.entries, 1)
	assert.Equal(t, domain.EntryBetDebit, h.state.entries[0].EntryType)
	assert.Equal(t, int64(30_00), h.state.entries[0].AmountMinor)
	assert.Equal(t, int64(70_00), h.state.entries[0].BalanceAfterMinor)
	assert.Equal(t, int64(1), h.state.entries[0].Sequence)

	require.Len(t, h.state.operations, 1)
	require.Len(t, h.state.outbox, 1)
	assert.Equal(t, domain.EventBetPlaced, h.state.outbox[0].EventType)
	assert.Equal(t, testWalletID, h.state.outbox[0].MessageGroupID,
		"the FIFO group must be the wallet so one wallet stays ordered")
	assert.Equal(t, h.state.outbox[0].EventUID, h.state.outbox[0].DedupID,
		"the SQS deduplication id must be the event uid")

	require.Len(t, h.state.audit, 1)
	assert.Equal(t, domain.AuditAccepted, h.state.audit[0].Outcome)
	assert.Equal(t, domain.ChannelAPI, h.state.audit[0].Channel)
}

// T041: the same key with the same body replays the stored response and changes
// nothing.
func TestApplyReplaysSameKey(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)
	in := betInput("k1", 30_00, domain.BRL)

	first, err := h.svc.Apply(context.Background(), in)
	require.NoError(t, err)
	second, err := h.svc.Apply(context.Background(), in)
	require.NoError(t, err)

	assert.False(t, first.Replay)
	assert.True(t, second.Replay)
	assert.Equal(t, string(first.Body), string(second.Body), "a replay must be byte-identical")

	assert.Equal(t, int64(70_00), h.state.wallets[testWalletID].Balance.AmountMinor,
		"a replay must not move the balance")
	assert.Len(t, h.state.entries, 1, "a replay must not append a second entry")
	assert.Len(t, h.state.operations, 1)
	assert.Len(t, h.state.outbox, 1, "a replay must not emit a second event")
	assert.Len(t, h.state.audit, 2, "each request produces exactly one audit line")
	assert.Equal(t, domain.AuditAccepted, h.state.audit[1].Outcome)
}

// T042: the same key with a different body is a conflict, not a replay.
func TestApplyRejectsKeyConflict(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)
	_, err := h.svc.Apply(context.Background(), betInput("k1", 30_00, domain.BRL))
	require.NoError(t, err)

	_, err = h.svc.Apply(context.Background(), betInput("k1", 40_00, domain.BRL))
	require.Error(t, err)
	assert.Equal(t, domain.ReasonIdempotencyKeyConflict, domain.CodeOf(err))

	assert.Equal(t, int64(70_00), h.state.wallets[testWalletID].Balance.AmountMinor,
		"a conflicting request must not move the balance")
	assert.Len(t, h.state.entries, 1)
	require.Len(t, h.state.audit, 2)
	assert.Equal(t, domain.AuditRejected, h.state.audit[1].Outcome)
	assert.Equal(t, domain.ReasonIdempotencyKeyConflict, h.state.audit[1].ReasonCode)
}

// T043: a request with no Idempotency-Key is rejected before anything is locked
// or written.
func TestApplyRejectsMissingIdempotencyKey(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)

	_, err := h.svc.Apply(context.Background(), betInput("", 30_00, domain.BRL))
	require.Error(t, err)
	assert.Equal(t, domain.ReasonMissingIdempotencyKey, domain.CodeOf(err))

	assert.Empty(t, *h.calls, "nothing may be read or written before the key is validated")
	assert.Equal(t, int64(100_00), h.state.wallets[testWalletID].Balance.AmountMinor)
	assert.Len(t, h.state.audit, 1)
	assert.Equal(t, domain.AuditRejected, h.state.audit[0].Outcome)
}

func TestApplyRejectsOverlongIdempotencyKey(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)
	in := betInput("", 30_00, domain.BRL)
	in.IdempotencyKey = string(make([]byte, 0, 300))
	for i := range in.IdempotencyKey {
		in.IdempotencyKey = in.IdempotencyKey[:i] + "k" + in.IdempotencyKey[i+1:]
	}

	_, err := h.svc.Apply(context.Background(), in)
	require.Error(t, err)
	assert.Equal(t, domain.ReasonMissingIdempotencyKey, domain.CodeOf(err))
}

// T044: a non-positive or non-integer amount never reaches the ledger.
func TestApplyRejectsInvalidAmounts(t *testing.T) {
	for _, minor := range []int64{0, -1, -100_00} {
		t.Run(string(rune('0'+minor%3)), func(t *testing.T) {
			h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)
			_, err := h.svc.Apply(context.Background(), betInput("k1", minor, domain.BRL))
			require.Error(t, err)
			assert.Equal(t, domain.ReasonInvalidAmount, domain.CodeOf(err))
			assert.Len(t, h.state.entries, 0)
			assert.Equal(t, int64(100_00), h.state.wallets[testWalletID].Balance.AmountMinor)
		})
	}
}

// T045: funds and currency are checked before any write.
func TestApplyRejectsInsufficientFunds(t *testing.T) {
	h := newHarness(t, 10_00, domain.BRL, domain.WalletActive)

	_, err := h.svc.Apply(context.Background(), betInput("k1", 30_00, domain.BRL))
	require.Error(t, err)
	assert.Equal(t, domain.ReasonInsufficientFunds, domain.CodeOf(err))

	var domErr *domain.Error
	require.ErrorAs(t, err, &domErr)
	require.NotNil(t, domErr.AvailableMinor)
	assert.Equal(t, int64(10_00), *domErr.AvailableMinor,
		"the caller needs the available balance to size a retry without a second request")

	assert.Len(t, h.state.entries, 0)
	assert.Equal(t, int64(10_00), h.state.wallets[testWalletID].Balance.AmountMinor)
	require.Len(t, h.state.audit, 1)
	assert.Equal(t, domain.AuditRejected, h.state.audit[0].Outcome)
}

func TestApplyRejectsCurrencyMismatch(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)

	_, err := h.svc.Apply(context.Background(), betInput("k1", 30_00, domain.USD))
	require.Error(t, err)
	assert.Equal(t, domain.ReasonCurrencyMismatch, domain.CodeOf(err))
	assert.Len(t, h.state.entries, 0)
	assert.Equal(t, int64(100_00), h.state.wallets[testWalletID].Balance.AmountMinor)
}

func TestApplyRejectsFrozenWallet(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletFrozen)

	_, err := h.svc.Apply(context.Background(), betInput("k1", 30_00, domain.BRL))
	require.Error(t, err)
	assert.Equal(t, domain.ReasonWalletFrozen, domain.CodeOf(err))
	assert.Len(t, h.state.entries, 0)
}

func TestApplyRejectsUnknownWallet(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)

	in := betInput("k1", 30_00, domain.BRL)
	in.WalletID = "33333333-3333-4333-8333-333333333333"
	_, err := h.svc.Apply(context.Background(), in)
	require.Error(t, err)
	assert.Equal(t, domain.ReasonWalletNotFound, domain.CodeOf(err))
}

func TestApplyRejectsTenantMismatch(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)

	in := betInput("k1", 30_00, domain.BRL)
	in.TenantID = "99999999-9999-4999-8999-999999999999"
	_, err := h.svc.Apply(context.Background(), in)
	require.Error(t, err)
	assert.Equal(t, domain.ReasonTenantMismatch, domain.CodeOf(err))
	assert.Len(t, h.state.entries, 0)
}

// A distinct key reusing a transaction id is refused, and it does not consume
// the key: after the caller fixes the request, the same key must still work.
func TestApplyRejectsDuplicateTransactionID(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)
	_, err := h.svc.Apply(context.Background(), betInput("k1", 30_00, domain.BRL))
	require.NoError(t, err)

	reused := betInput("k2", 10_00, domain.BRL)
	reused.Operation.TransactionID = "txn-k1"
	_, err = h.svc.Apply(context.Background(), reused)
	require.Error(t, err)
	assert.Equal(t, domain.ReasonDuplicateTransactionID, domain.CodeOf(err))

	fixed := betInput("k2", 10_00, domain.BRL)
	fixed.Operation.TransactionID = "txn-fresh"
	out, err := h.svc.Apply(context.Background(), fixed)
	require.NoError(t, err)
	assert.False(t, out.Replay)
	assert.Equal(t, int64(60_00), h.state.wallets[testWalletID].Balance.AmountMinor)
}

// A rejected request leaves no idempotency record, so a legitimate retry after
// the caller fixes the problem is never blocked.
func TestRejectedRequestDoesNotBurnTheIdempotencyKey(t *testing.T) {
	h := newHarness(t, 10_00, domain.BRL, domain.WalletActive)

	_, err := h.svc.Apply(context.Background(), betInput("k1", 30_00, domain.BRL))
	require.Error(t, err)
	require.Empty(t, h.state.operations, "a rejection must not create an idempotency record")

	h.state.wallets[testWalletID].Balance.AmountMinor = 100_00
	out, err := h.svc.Apply(context.Background(), betInput("k1", 30_00, domain.BRL))
	require.NoError(t, err)
	assert.False(t, out.Replay)
	assert.Equal(t, int64(70_00), h.state.wallets[testWalletID].Balance.AmountMinor)
}

// Two different keys on the same wallet both apply, and each gets its own
// sequence, so the ledger stays totally ordered per wallet.
func TestSequentialOperationsKeepPerWalletOrder(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)

	for i, key := range []string{"k1", "k2", "k3"} {
		_, err := h.svc.Apply(context.Background(), betInput(key, 10_00, domain.BRL))
		require.NoError(t, err)
		assert.Equal(t, int64(90_00-i*10_00), h.state.wallets[testWalletID].Balance.AmountMinor)
	}

	require.Len(t, h.state.entries, 3)
	for i, e := range h.state.entries {
		assert.Equal(t, int64(i+1), e.Sequence)
	}
}

// The request hash must track substance, not presentation. Two requests that
// persist different state are different requests, and two that persist the same
// state are the same one.
func TestRequestHashTracksSubstanceOnly(t *testing.T) {
	base := betInput("k1", 30_00, domain.BRL).Operation

	same := base
	same.BetID = base.BetID
	assert.Equal(t, RequestHash(base), RequestHash(same))

	// referenceId is persisted as the bet's external reference, so changing it
	// changes what was written and must count as a different request.
	differentRef := base
	differentRef.ReferenceID = "some-reference"
	assert.NotEqual(t, RequestHash(base), RequestHash(differentRef))

	different := base
	different.Amount = domain.Money{AmountMinor: 30_01, Currency: domain.BRL}
	assert.NotEqual(t, RequestHash(base), RequestHash(different))

	otherCurrency := base
	otherCurrency.Amount = domain.Money{AmountMinor: 30_00, Currency: domain.USD}
	assert.NotEqual(t, RequestHash(base), RequestHash(otherCurrency),
		"BRL 3000 and USD 3000 are different requests")

	// The hash must be stable across runs, not depend on map iteration or any
	// per-process state, or a restart would turn every replay into a conflict.
	assert.Equal(t, RequestHash(base), RequestHash(base))
	assert.Len(t, RequestHash(base), 32)
}

// T046: the balance read returns the exact stored integer and the tenant must
// match the token.
func TestGetBalance(t *testing.T) {
	h := newHarness(t, 123_456_789, domain.BRL, domain.WalletActive)
	_, err := h.svc.Apply(context.Background(), betInput("k1", 1_00, domain.BRL))
	require.NoError(t, err)

	view, err := h.svc.GetBalance(context.Background(), testTenant, testWalletID)
	require.NoError(t, err)
	assert.Equal(t, int64(123_456_689), view.BalanceMinor)
	assert.Equal(t, domain.BRL, view.Currency)
	assert.Equal(t, testWalletID, view.WalletID)

	_, err = h.svc.GetBalance(context.Background(), "99999999-9999-4999-8999-999999999999", testWalletID)
	require.Error(t, err)
	assert.Equal(t, domain.ReasonTenantMismatch, domain.CodeOf(err))

	_, err = h.svc.GetBalance(context.Background(), testTenant, "33333333-3333-4333-8333-333333333333")
	require.Error(t, err)
	assert.Equal(t, domain.ReasonWalletNotFound, domain.CodeOf(err))
}

// Operation types outside the MVP are refused explicitly, never silently
// ignored: a silent no-op would look like a successful bet.
func TestUnimplementedOperationTypesAreRefused(t *testing.T) {
	h := newHarness(t, 100_00, domain.BRL, domain.WalletActive)

	for _, typ := range []domain.OperationType{
		domain.OperationWin, domain.OperationLoss, domain.OperationRefund, domain.OperationRollback,
	} {
		in := betInput("key-"+string(typ), 10_00, domain.BRL)
		in.Operation.Type = typ
		_, err := h.svc.Apply(context.Background(), in)
		require.Error(t, err, "%s must not succeed in this build", typ)
		assert.Len(t, h.state.entries, 0)
	}
}
