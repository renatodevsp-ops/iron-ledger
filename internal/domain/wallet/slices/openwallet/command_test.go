package openwallet

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/domain/wallet/domain"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/events"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

var (
	walletID  = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	playerID  = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	openingTx = uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d")
	brl       = money.Currency("BRL")
	at        = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
)

func command(initial string) Command {
	return Command{
		WalletID:  walletID,
		PlayerID:  playerID,
		OpeningTx: openingTx,
		Initial:   money.MustParse(initial, brl),
	}
}

func envelope(event cqrs.Event) *cqrs.Envelope {
	return &cqrs.Envelope{Event: event, OccurredAt: at, StreamID: walletID.String(), Version: 1}
}

func Test_decide(t *testing.T) {
	type args struct {
		history []*cqrs.Envelope
		cmd     Command
	}

	tests := []struct {
		name        string
		args        args
		checkEvents func(t *testing.T, got []cqrs.Event)
		wantErr     bool
		wantCode    xerr.Code
	}{
		{
			name: "spec: Open Wallet - opens a wallet with a balance, crediting it and recording the credit",
			args: args{cmd: command("1000.00")},
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				opened, ok := got[0].(*events.WalletOpened)
				if !ok {
					t.Fatalf("first event = %T, want *events.WalletOpened", got[0])
				}
				if opened.WalletID != walletID || opened.PlayerID != playerID || opened.OpeningTx != openingTx {
					t.Errorf("WalletOpened = %+v", opened)
				}
				if !opened.Balance.Equal(money.MustParse("1000.00", brl)) {
					t.Errorf("opening balance = %s, want 1000.00", opened.Balance)
				}

				changed, ok := got[1].(*events.WalletBalanceChanged)
				if !ok {
					t.Fatalf("second event = %T, want *events.WalletBalanceChanged", got[1])
				}
				if changed.Direction != domain.DirectionCredit {
					t.Errorf("direction = %s, want CREDIT", changed.Direction)
				}
				if !changed.BalanceBefore.IsZero() {
					t.Errorf("balanceBefore = %s, want 0.00", changed.BalanceBefore)
				}
				if !changed.BalanceAfter.Equal(money.MustParse("1000.00", brl)) {
					t.Errorf("balanceAfter = %s, want 1000.00", changed.BalanceAfter)
				}
				if changed.WalletVersion != domain.InitialVersion {
					t.Errorf("walletVersion = %d, want %d", changed.WalletVersion, domain.InitialVersion)
				}
				if changed.TransactionID != openingTx {
					t.Errorf("transactionId = %s, want the opening transaction %s", changed.TransactionID, openingTx)
				}
			},
		},
		{
			name: "spec: Open Wallet - opens a wallet at zero without recording any movement",
			args: args{cmd: command("0.00")},
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				if len(got) != 1 {
					t.Fatalf("got %d events, want 1: a wallet opened at zero moves no money", len(got))
				}
				opened, ok := got[0].(*events.WalletOpened)
				if !ok {
					t.Fatalf("event = %T, want *events.WalletOpened", got[0])
				}
				if !opened.Balance.IsZero() {
					t.Errorf("balance = %s, want 0.00", opened.Balance)
				}
			},
		},
		{
			name:     "spec: Open Wallet - refuses a negative opening balance",
			args:     args{cmd: command("-1.00")},
			wantErr:  true,
			wantCode: xerr.CodeNegativeAmountNotAllowed,
		},
		{
			name: "spec: Open Wallet - settles in the currency of the opening balance",
			args: args{
				cmd: Command{
					WalletID:  walletID,
					PlayerID:  playerID,
					OpeningTx: openingTx,
					Initial:   money.MustParse("1000.00", "USD"),
				},
			},
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				opened, ok := got[0].(*events.WalletOpened)
				if !ok {
					t.Fatalf("first event = %T, want *events.WalletOpened", got[0])
				}
				// The wallet settles in exactly one currency, and it is the one
				// the opening balance is denominated in. There is no second field
				// that could disagree with it.
				if opened.Balance.Currency() != "USD" {
					t.Errorf("currency = %s, want USD", opened.Balance.Currency())
				}
			},
		},
		{
			name: "spec: Open Wallet - refuses an opening without a transaction identity",
			args: args{
				cmd: Command{
					WalletID: walletID,
					PlayerID: playerID,
					Initial:  money.MustParse("1000.00", brl),
				},
			},
			wantErr:  true,
			wantCode: xerr.CodeInvalidRequest,
		},
		{
			name: "spec: Open Wallet - refuses to open a wallet that already exists",
			args: args{
				history: []*cqrs.Envelope{
					envelope(&events.WalletOpened{
						WalletID:  walletID,
						PlayerID:  playerID,
						OpeningTx: openingTx,
						Balance:   money.MustParse("1000.00", brl),
					}),
				},
				cmd: command("1000.00"),
			},
			wantErr:  true,
			wantCode: xerr.CodeWalletAlreadyExists,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := initialState()
			for _, e := range tt.args.history {
				s = evolve(s, e)
			}

			got, err := decide(s, tt.args.cmd)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decide() succeeded, want rejection %s", tt.wantCode)
				}
				if got != nil {
					t.Errorf("decide() returned %d events alongside an error, want none", len(got))
				}
				if code, _ := xerr.CodeOf(err); code != tt.wantCode {
					t.Errorf("error code = %v, want %v", code, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("decide() unexpected error: %v", err)
			}
			tt.checkEvents(t, got)
		})
	}
}

func Test_evolve_foldsTheStreamIntoState(t *testing.T) {
	s := initialState()
	s = evolve(s, envelope(&events.WalletOpened{
		WalletID:  walletID,
		PlayerID:  playerID,
		OpeningTx: openingTx,
		Balance:   money.MustParse("1000.00", brl),
	}))
	if !s.exists {
		t.Fatal("state does not know the wallet exists")
	}
	if s.snapshot.Version != domain.InitialVersion {
		t.Errorf("version = %d, want %d", s.snapshot.Version, domain.InitialVersion)
	}

	s = evolve(s, envelope(&events.WalletBalanceChanged{
		WalletID:      walletID,
		TransactionID: openingTx,
		Direction:     domain.DirectionCredit,
		Money:         money.MustParse("1000.00", brl),
		BalanceBefore: money.Zero(brl),
		BalanceAfter:  money.MustParse("1000.00", brl),
		WalletVersion: domain.InitialVersion,
	}))
	if s.snapshot.Balance.String() != "1000.00" {
		t.Errorf("balance = %s, want 1000.00", s.snapshot.Balance)
	}
}

func Test_decide_eventsSerialiseWithDecimalMoney(t *testing.T) {
	got, err := decide(initialState(), command("1234.56"))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"amount":"1234.56"`)) {
		t.Errorf("encoded events = %s, want a decimal amount string", encoded)
	}
	// Every amount must be a quoted decimal string: an unquoted number would mean
	// a float had entered the pipeline.
	if bytes.Contains(encoded, []byte(`:1234`)) {
		t.Errorf("encoded events = %s, want every amount quoted as a decimal string", encoded)
	}
}
