package applywalletmovement

import (
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
	walletID = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	playerID = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	txID     = uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d")
	brl      = money.Currency("BRL")
	at       = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
)

func opened(balance string) []*cqrs.Envelope {
	return []*cqrs.Envelope{
		{
			Event: &events.WalletOpened{
				WalletID:  walletID,
				PlayerID:  playerID,
				OpeningTx: txID,
				Balance:   money.MustParse(balance, brl),
			},
			OccurredAt: at,
			StreamID:   walletID.String(),
			Version:    1,
		},
	}
}

func command(direction domain.Direction, amount string) Command {
	return Command{
		WalletID:      walletID,
		TransactionID: txID,
		Direction:     direction,
		Money:         money.MustParse(amount, brl),
	}
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
			name: "spec: Apply Wallet Movement - debits the balance and records the movement",
			args: args{history: opened("100.00"), cmd: command(domain.DirectionDebit, "25.00")},
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				changed, ok := got[0].(*events.WalletBalanceChanged)
				if !ok {
					t.Fatalf("event = %T, want *events.WalletBalanceChanged", got[0])
				}
				if !changed.BalanceBefore.Equal(money.MustParse("100.00", brl)) {
					t.Errorf("balanceBefore = %s, want 100.00", changed.BalanceBefore)
				}
				if !changed.BalanceAfter.Equal(money.MustParse("75.00", brl)) {
					t.Errorf("balanceAfter = %s, want 75.00", changed.BalanceAfter)
				}
				if changed.WalletVersion != 2 {
					t.Errorf("walletVersion = %d, want 2", changed.WalletVersion)
				}
			},
		},
		{
			name: "spec: Apply Wallet Movement - credits the balance and records the movement",
			args: args{history: opened("100.00"), cmd: command(domain.DirectionCredit, "10.00")},
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				changed := got[0].(*events.WalletBalanceChanged)
				if changed.Direction != domain.DirectionCredit {
					t.Errorf("direction = %s, want CREDIT", changed.Direction)
				}
				if !changed.BalanceAfter.Equal(money.MustParse("110.00", brl)) {
					t.Errorf("balanceAfter = %s, want 110.00", changed.BalanceAfter)
				}
			},
		},
		{
			name:     "spec: Apply Wallet Movement - refuses a debit one cent beyond the balance",
			args:     args{history: opened("100.00"), cmd: command(domain.DirectionDebit, "100.01")},
			wantErr:  true,
			wantCode: xerr.CodeInsufficientBalance,
		},
		{
			name:     "spec: Apply Wallet Movement - refuses a debit from an empty wallet",
			args:     args{history: opened("0.00"), cmd: command(domain.DirectionDebit, "0.01")},
			wantErr:  true,
			wantCode: xerr.CodeInsufficientBalance,
		},
		{
			name: "spec: Apply Wallet Movement - refuses a movement in another currency",
			args: args{
				history: opened("100.00"),
				cmd: Command{
					WalletID:      walletID,
					TransactionID: txID,
					Direction:     domain.DirectionCredit,
					Money:         money.MustParse("10.00", "USD"),
				},
			},
			wantErr:  true,
			wantCode: xerr.CodeCurrencyMismatch,
		},
		{
			name: "spec: Apply Wallet Movement - refuses a movement with no transaction to tie it to",
			args: args{
				history: opened("100.00"),
				cmd: Command{
					WalletID:  walletID,
					Direction: domain.DirectionCredit,
					Money:     money.MustParse("10.00", brl),
				},
			},
			wantErr:  true,
			wantCode: xerr.CodeInvalidRequest,
		},
		{
			name:     "spec: Apply Wallet Movement - refuses a wallet that does not exist",
			args:     args{cmd: command(domain.DirectionCredit, "10.00")},
			wantErr:  true,
			wantCode: xerr.CodeWalletNotFound,
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
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			tt.checkEvents(t, got)
		})
	}
}

func Test_decide_versionFollowsTheBalance(t *testing.T) {
	s := initialState()
	for _, e := range opened("100.00") {
		s = evolve(s, e)
	}

	// Two movements in the same stream: the version tracks the number of
	// balance changes, never the number of commands.
	for i := 1; i <= 2; i++ {
		got, err := decide(s, command(domain.DirectionDebit, "10.00"))
		if err != nil {
			t.Fatalf("decide: %v", err)
		}
		changed := got[0].(*events.WalletBalanceChanged)
		if changed.WalletVersion != int64(i+1) {
			t.Fatalf("walletVersion = %d, want %d", changed.WalletVersion, i+1)
		}
		s = evolve(s, &cqrs.Envelope{Event: changed, OccurredAt: at})
	}
}
