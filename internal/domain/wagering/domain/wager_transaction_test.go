package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

var (
	txID     = uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d")
	walletID = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	playerID = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	brl      = money.Currency("BRL")
	at       = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
)

func betRegistration() Registration {
	return Registration{
		ID:             txID,
		Origin:         OriginExternal,
		ProviderID:     "provider-a",
		ExternalID:     "transaction-123",
		IdempotencyKey: "provider-a:transaction-123",
		PayloadHash:    "abc",
		WalletID:       walletID,
		PlayerID:       playerID,
		RoundID:        "round-987",
		GameID:         "fortune-chimp",
		Kind:           KindBet,
		Amount:         money.MustParse("25.00", brl),
		RecordedAt:     at,
	}
}

func registered(t *testing.T) WagerTransaction {
	t.Helper()
	tx, err := Register(betRegistration(), at)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return tx
}

func TestRegister_startsPending(t *testing.T) {
	tx := registered(t)
	if tx.Status() != StatusPending {
		t.Errorf("status = %s, want %s", tx.Status(), StatusPending)
	}
	if tx.BalanceAfter() != nil {
		t.Error("a newly registered transaction must not carry a resulting balance")
	}
	if tx.ReferenceInternalID() != nil {
		t.Error("a newly registered transaction must not carry a resolved reference")
	}
}

func TestRegister_rejectsIncompleteOperations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Registration)
		code   xerr.Code
	}{
		{"missing identity", func(r *Registration) { r.ID = uuid.Nil }, xerr.CodeInvalidRequest},
		{"missing wallet", func(r *Registration) { r.WalletID = uuid.Nil }, xerr.CodeInvalidRequest},
		{"missing player", func(r *Registration) { r.PlayerID = uuid.Nil }, xerr.CodeInvalidRequest},
		{"missing provider", func(r *Registration) { r.ProviderID = "" }, xerr.CodeInvalidRequest},
		{"missing external id", func(r *Registration) { r.ExternalID = "" }, xerr.CodeInvalidRequest},
		{"missing idempotency key", func(r *Registration) { r.IdempotencyKey = "" }, xerr.CodeInvalidRequest},
		{"zero bet", func(r *Registration) { r.Amount = money.MustParse("0.00", brl) }, xerr.CodeAmountMustBePositive},
		{"negative bet", func(r *Registration) { r.Amount = money.MustParse("-1.00", brl) }, xerr.CodeAmountMustBePositive},
		{"missing currency", func(r *Registration) { r.Amount = money.Money{} }, xerr.CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registration := betRegistration()
			tt.mutate(&registration)
			_, err := Register(registration, at)
			if code, _ := xerr.CodeOf(err); code != tt.code {
				t.Errorf("error code = %v, want %v (err %v)", code, tt.code, err)
			}
		})
	}
}

func TestParseKind(t *testing.T) {
	tests := []struct {
		raw     string
		want    Kind
		wantErr bool
	}{
		{raw: "BET", want: KindBet},
		{raw: "WIN", want: KindWin},
		{raw: "LOSS", want: KindLoss},
		{raw: "REFUND", want: KindRefund},
		{raw: "ROLLBACK", want: KindRollback},
		{raw: "OPENING", wantErr: true},
		{raw: "bet", wantErr: true},
		{raw: "", wantErr: true},
		{raw: "TRANSFER", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := ParseKind(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseKind(%q) = %s, want rejection", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseKind(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("ParseKind(%q) = %s, want %s", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseKindInternal_allowsOpening(t *testing.T) {
	if got, err := ParseKindInternal("OPENING"); err != nil || got != KindOpening {
		t.Errorf("ParseKindInternal(OPENING) = %s, %v", got, err)
	}
	if _, err := ParseKindInternal("TRANSFER"); err == nil {
		t.Error("ParseKindInternal accepted an unknown kind")
	}
}

func TestKind_ValidateAmount(t *testing.T) {
	tests := []struct {
		kind   Kind
		amount string
		code   xerr.Code
	}{
		{kind: KindBet, amount: "25.00"},
		{kind: KindBet, amount: "0.00", code: xerr.CodeAmountMustBePositive},
		{kind: KindWin, amount: "0.01"},
		{kind: KindWin, amount: "0.00", code: xerr.CodeAmountMustBePositive},
		{kind: KindLoss, amount: "0.00"},
		{kind: KindLoss, amount: "1.00", code: xerr.CodeLossAmountMustBeZero},
		{kind: KindRefund, amount: "25.00"},
		{kind: KindRefund, amount: "0.00", code: xerr.CodeAmountMustBePositive},
		{kind: KindRollback, amount: "25.00"},
		{kind: KindRollback, amount: "0.00", code: xerr.CodeAmountMustBePositive},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind)+"/"+tt.amount, func(t *testing.T) {
			err := tt.kind.ValidateAmount(money.MustParse(tt.amount, brl))
			if tt.code == "" {
				if err != nil {
					t.Fatalf("unexpected rejection: %v", err)
				}
				return
			}
			if code, _ := xerr.CodeOf(err); code != tt.code {
				t.Errorf("error code = %v, want %v", code, tt.code)
			}
		})
	}
}

func TestKind_requiresReference(t *testing.T) {
	if !KindRefund.RequiresReference() || !KindRollback.RequiresReference() {
		t.Error("REFUND and ROLLBACK must name a reference")
	}
	for _, kind := range []Kind{KindBet, KindWin, KindLoss} {
		if kind.RequiresReference() {
			t.Errorf("%s must not require a reference", kind)
		}
	}
	registration := betRegistration()
	registration.Kind = KindRefund
	registration.ReferenceExternalID = nil
	if _, err := Register(registration, at); err == nil {
		t.Error("Register accepted a REFUND with no reference")
	}
}

func TestKind_movesMoneyAndDirection(t *testing.T) {
	tests := []struct {
		kind      Kind
		moves     bool
		direction string
	}{
		{KindOpening, true, "CREDIT"},
		{KindBet, true, "DEBIT"},
		{KindWin, true, "CREDIT"},
		{KindLoss, false, ""},
		{KindRefund, true, "CREDIT"},
		{KindRollback, true, ""},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			if tt.kind.MovesMoney() != tt.moves {
				t.Errorf("MovesMoney() = %t, want %t", tt.kind.MovesMoney(), tt.moves)
			}
			direction, ok := tt.kind.Direction()
			if ok != (tt.direction != "") {
				t.Errorf("Direction() ok = %t, want %t", ok, tt.direction != "")
			}
			if ok && direction != tt.direction {
				t.Errorf("Direction() = %s, want %s", direction, tt.direction)
			}
		})
	}
}

func TestKind_referenceEligibility(t *testing.T) {
	if !IsRefundable(KindBet) {
		t.Error("a REFUND must be able to reference a BET")
	}
	if IsRefundable(KindWin) {
		t.Error("a REFUND must not be able to reference a WIN")
	}
	for _, kind := range []Kind{KindBet, KindWin, KindRefund} {
		if !IsReversible(kind) {
			t.Errorf("a ROLLBACK must be able to reference a %s", kind)
		}
	}
	if IsReversible(KindLoss) {
		t.Error("a ROLLBACK must not be able to reference a LOSS")
	}
}

func TestSettle(t *testing.T) {
	tx := registered(t)
	settled, err := tx.Settle(nil, money.MustParse("75.00", brl), at.Add(time.Second))
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if settled.Status() != StatusProcessed {
		t.Errorf("status = %s, want %s", settled.Status(), StatusProcessed)
	}
	if settled.BalanceAfter() == nil || settled.BalanceAfter().String() != "75.00" {
		t.Errorf("balanceAfter = %v, want 75.00", settled.BalanceAfter())
	}
	if !settled.UpdatedAt().Equal(at.Add(time.Second)) {
		t.Errorf("updatedAt = %s, want %s", settled.UpdatedAt(), at.Add(time.Second))
	}
}

func TestSettle_refusesAnotherCurrency(t *testing.T) {
	tx := registered(t)
	if _, err := tx.Settle(nil, money.MustParse("75.00", "USD"), at); err == nil {
		t.Fatal("Settle accepted a balance in another currency")
	}
}

func TestAwaitReference(t *testing.T) {
	registration := betRegistration()
	reference := "transaction-999"
	registration.Kind = KindRollback
	registration.ReferenceExternalID = &reference
	withReference, err := Register(registration, at)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	waiting, err := withReference.AwaitReference(at.Add(30*time.Second), "not received yet", at)
	if err != nil {
		t.Fatalf("AwaitReference: %v", err)
	}
	if waiting.Status() != StatusPendingReference {
		t.Errorf("status = %s, want %s", waiting.Status(), StatusPendingReference)
	}
	if waiting.PendingAttempts() != 1 {
		t.Errorf("attempts = %d, want 1", waiting.PendingAttempts())
	}
	if waiting.NextAttemptAt() == nil || !waiting.NextAttemptAt().Equal(at.Add(30*time.Second)) {
		t.Errorf("nextAttemptAt = %v, want %s", waiting.NextAttemptAt(), at.Add(30*time.Second))
	}

	// Rescheduling accumulates attempts, which is what the TTL counts.
	again, err := waiting.AwaitReference(at.Add(time.Minute), "still not received", at)
	if err != nil {
		t.Fatalf("AwaitReference: %v", err)
	}
	if again.PendingAttempts() != 2 {
		t.Errorf("attempts = %d, want 2", again.PendingAttempts())
	}
}

func TestAwaitReference_needsAReference(t *testing.T) {
	if _, err := registered(t).AwaitReference(at.Add(time.Second), "why", at); err == nil {
		t.Error("AwaitReference accepted a transaction with no reference")
	}
}

func TestTerminalStatesAreTerminal(t *testing.T) {
	tests := []struct {
		name       string
		transition func(WagerTransaction) (WagerTransaction, error)
	}{
		{"settle", func(tx WagerTransaction) (WagerTransaction, error) {
			return tx.Settle(nil, money.MustParse("1.00", brl), at)
		}},
		{"reject", func(tx WagerTransaction) (WagerTransaction, error) {
			return tx.Reject(xerr.CodeInsufficientBalance, "no money", at)
		}},
		{"fail", func(tx WagerTransaction) (WagerTransaction, error) {
			return tx.Fail(xerr.CodeInfrastructureFailure, "database down", at)
		}},
		{"await reference", func(tx WagerTransaction) (WagerTransaction, error) {
			return tx.AwaitReference(at, "waiting", at)
		}},
	}

	for _, terminal := range []struct {
		name       string
		transition func(WagerTransaction) (WagerTransaction, error)
	}{
		{"processed", tests[0].transition},
		{"rejected", tests[1].transition},
		{"failed", tests[2].transition},
	} {
		t.Run("after "+terminal.name, func(t *testing.T) {
			settled, err := terminal.transition(registered(t))
			if err != nil {
				t.Fatalf("first transition: %v", err)
			}
			for _, tt := range tests {
				if _, err := tt.transition(settled); err == nil {
					t.Errorf("%s was accepted on a %s transaction", tt.name, terminal.name)
				}
			}
		})
	}
}

func TestRejectAndFail(t *testing.T) {
	rejected, err := registered(t).Reject(xerr.CodeInsufficientBalance, "the wallet holds 0.00", at)
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if rejected.Status() != StatusRejected || rejected.FailureCode() != xerr.CodeInsufficientBalance {
		t.Errorf("status = %s code = %s", rejected.Status(), rejected.FailureCode())
	}
	if rejected.FailureMessage() != "the wallet holds 0.00" {
		t.Errorf("message = %q", rejected.FailureMessage())
	}

	failed, err := registered(t).Fail(xerr.CodeInfrastructureFailure, "the database was down", at)
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if failed.Status() != StatusFailed || failed.FailureCode() != xerr.CodeInfrastructureFailure {
		t.Errorf("status = %s code = %s", failed.Status(), failed.FailureCode())
	}
}

func TestStatus_terminal(t *testing.T) {
	terminal := []Status{StatusProcessed, StatusRejected, StatusFailed}
	open := []Status{StatusPending, StatusPendingReference}
	for _, status := range terminal {
		if !status.Terminal() {
			t.Errorf("%s should be terminal", status)
		}
	}
	for _, status := range open {
		if status.Terminal() {
			t.Errorf("%s should not be terminal", status)
		}
	}
}

func TestRehydrate(t *testing.T) {
	original := registered(t)
	rehydrated, err := Rehydrate(original.Snapshot())
	if err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	if rehydrated.Status() != original.Status() || !rehydrated.Amount().Equal(original.Amount()) {
		t.Errorf("rehydrated = %s/%s", rehydrated.Status(), rehydrated.Amount())
	}

	broken := original.Snapshot()
	broken.Status = "MAYBE"
	if _, err := Rehydrate(broken); err == nil {
		t.Error("Rehydrate accepted an unknown status")
	}
}

func TestRegister_isImmutable(t *testing.T) {
	tx := registered(t)
	if _, err := tx.Reject(xerr.CodeInsufficientBalance, "no", at); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if tx.Status() != StatusPending {
		t.Errorf("the original transaction changed to %s", tx.Status())
	}
}
