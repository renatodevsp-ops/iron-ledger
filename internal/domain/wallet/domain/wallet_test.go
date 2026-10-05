package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

var (
	walletID = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	playerID = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	txID     = uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d")
	otherTx  = uuid.MustParse("0192f299-345e-7e38-af88-e43f851a819d")
	brl      = money.Currency("BRL")
	at       = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
)

func open(t *testing.T, initial string) Wallet {
	t.Helper()
	wallet, err := Open(walletID, playerID, brl, money.MustParse(initial, brl), at)
	if err != nil {
		t.Fatalf("Open(%s): %v", initial, err)
	}
	return wallet
}

func TestOpen(t *testing.T) {
	tests := []struct {
		name      string
		initial   string
		want      string
		version   int64
		wantError bool
	}{
		{name: "positive balance", initial: "1000.00", want: "1000.00", version: InitialVersion},
		{name: "zero balance is valid", initial: "0.00", want: "0.00", version: InitialVersion},
		{name: "negative balance is refused", initial: "-0.01", wantError: true},
		{name: "one cent", initial: "0.01", want: "0.01", version: InitialVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wallet, err := Open(walletID, playerID, brl, money.MustParse(tt.initial, brl), at)
			if tt.wantError {
				if err == nil {
					t.Fatalf("Open(%s) succeeded, want rejection", tt.initial)
				}
				if code, ok := xerr.CodeOf(err); !ok || code != xerr.CodeNegativeAmountNotAllowed {
					t.Errorf("error code = %v, want %v", code, xerr.CodeNegativeAmountNotAllowed)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open(%s): %v", tt.initial, err)
			}
			if wallet.Balance().String() != tt.want {
				t.Errorf("balance = %s, want %s", wallet.Balance(), tt.want)
			}
			if wallet.Version() != tt.version {
				t.Errorf("version = %d, want %d", wallet.Version(), tt.version)
			}
			if wallet.Currency() != brl {
				t.Errorf("currency = %s, want %s", wallet.Currency(), brl)
			}
			if !wallet.CreatedAt().Equal(at) {
				t.Errorf("createdAt = %s, want %s", wallet.CreatedAt(), at)
			}
		})
	}
}

func TestOpen_rejectsIncompleteIdentity(t *testing.T) {
	tests := []struct {
		name     string
		id       uuid.UUID
		player   uuid.UUID
		currency money.Currency
	}{
		{"missing wallet id", uuid.Nil, playerID, brl},
		{"missing player id", walletID, uuid.Nil, brl},
		{"missing currency", walletID, playerID, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Open(tt.id, tt.player, tt.currency, money.Zero(brl), at); err == nil {
				t.Error("Open succeeded, want rejection")
			}
		})
	}
}

func TestWallet_Apply(t *testing.T) {
	tests := []struct {
		name        string
		initial     string
		direction   Direction
		amount      string
		wantBalance string
		wantVersion int64
		wantCode    xerr.Code
	}{
		{name: "debit reduces the balance", initial: "100.00", direction: DirectionDebit, amount: "25.00",
			wantBalance: "75.00", wantVersion: 2},
		{name: "credit increases the balance", initial: "100.00", direction: DirectionCredit, amount: "10.00",
			wantBalance: "110.00", wantVersion: 2},
		{name: "debit of the whole balance is allowed", initial: "100.00", direction: DirectionDebit, amount: "100.00",
			wantBalance: "0.00", wantVersion: 2},
		{name: "one cent over the balance is refused", initial: "100.00", direction: DirectionDebit, amount: "100.01",
			wantCode: xerr.CodeInsufficientBalance},
		{name: "debit on an empty wallet is refused", initial: "0.00", direction: DirectionDebit, amount: "0.01",
			wantCode: xerr.CodeInsufficientBalance},
		{name: "a zero movement is refused", initial: "100.00", direction: DirectionDebit, amount: "0.00",
			wantCode: xerr.CodeAmountMustBePositive},
		{name: "a negative movement is refused", initial: "100.00", direction: DirectionCredit, amount: "-1.00",
			wantCode: xerr.CodeAmountMustBePositive},
		{name: "an unknown direction is refused", initial: "100.00", direction: Direction("TRANSFER"), amount: "1.00",
			wantCode: xerr.CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wallet := open(t, tt.initial)
			updated, err := wallet.Apply(Movement{
				TransactionID: txID,
				Direction:     tt.direction,
				Amount:        money.MustParse(tt.amount, brl),
			}, at.Add(time.Minute))
			if tt.wantCode != "" {
				if err == nil {
					t.Fatalf("Apply succeeded, want rejection %s", tt.wantCode)
				}
				if code, _ := xerr.CodeOf(err); code != tt.wantCode {
					t.Errorf("error code = %v, want %v", code, tt.wantCode)
				}
				// The original wallet is untouched: a refused movement changes nothing.
				if wallet.Balance().String() != tt.initial {
					t.Errorf("balance changed to %s after a refusal", wallet.Balance())
				}
				if wallet.Version() != InitialVersion {
					t.Errorf("version changed to %d after a refusal", wallet.Version())
				}
				return
			}
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if updated.Balance().String() != tt.wantBalance {
				t.Errorf("balance = %s, want %s", updated.Balance(), tt.wantBalance)
			}
			if updated.Version() != tt.wantVersion {
				t.Errorf("version = %d, want %d", updated.Version(), tt.wantVersion)
			}
		})
	}
}

func TestWallet_ApplyRefusesAnotherCurrency(t *testing.T) {
	wallet := open(t, "100.00")
	_, err := wallet.Apply(Movement{
		TransactionID: txID,
		Direction:     DirectionCredit,
		Amount:        money.MustParse("10.00", "USD"),
	}, at)
	if code, _ := xerr.CodeOf(err); code != xerr.CodeCurrencyMismatch {
		t.Errorf("error code = %v, want %v", code, xerr.CodeCurrencyMismatch)
	}
}

func TestWallet_ApplyRequiresATransaction(t *testing.T) {
	wallet := open(t, "100.00")
	_, err := wallet.Apply(Movement{Direction: DirectionCredit, Amount: money.MustParse("1.00", brl)}, at)
	if code, _ := xerr.CodeOf(err); code != xerr.CodeInvalidRequest {
		t.Errorf("error code = %v, want %v", code, xerr.CodeInvalidRequest)
	}
}

func TestWallet_versionGrowsOnlyWithTheBalance(t *testing.T) {
	wallet := open(t, "100.00")
	for i := 0; i < 3; i++ {
		updated, err := wallet.Apply(Movement{
			TransactionID: otherTx,
			Direction:     DirectionCredit,
			Amount:        money.MustParse("1.00", brl),
		}, at)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		wallet = updated
	}
	if wallet.Version() != 4 {
		t.Errorf("version = %d, want 4 after three movements", wallet.Version())
	}
}

func TestWallet_rehydrateDoesNotMove(t *testing.T) {
	original := open(t, "100.00")

	rehydrated, err := Rehydrate(original.Snapshot())
	if err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	if !rehydrated.Balance().Equal(original.Balance()) {
		t.Errorf("balance = %s, want %s", rehydrated.Balance(), original.Balance())
	}
	if rehydrated.Version() != original.Version() {
		t.Errorf("version = %d, want %d", rehydrated.Version(), original.Version())
	}
}

func TestWallet_rehydrateRejectsBrokenState(t *testing.T) {
	snapshot := open(t, "100.00").Snapshot()
	snapshot.Version = 0
	if _, err := Rehydrate(snapshot); err == nil {
		t.Error("Rehydrate accepted a version below the initial version")
	}

	broken := open(t, "100.00").Snapshot()
	broken.ID = uuid.Nil
	if _, err := Rehydrate(broken); err == nil {
		t.Error("Rehydrate accepted a snapshot without an identity")
	}
}

func TestWallet_isImmutable(t *testing.T) {
	original := open(t, "100.00")
	updated, err := original.Apply(Movement{
		TransactionID: txID, Direction: DirectionDebit, Amount: money.MustParse("40.00", brl),
	}, at)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if original.Balance().String() != "100.00" {
		t.Errorf("the original wallet changed to %s", original.Balance())
	}
	if updated.Balance().String() != "60.00" {
		t.Errorf("the derived wallet is %s, want 60.00", updated.Balance())
	}
}
