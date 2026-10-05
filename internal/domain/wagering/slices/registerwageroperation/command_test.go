package registerwageroperation

import (
	"testing"
	"time"

	"github.com/google/uuid"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/events"
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

func command() Command {
	return Command{
		TransactionID:  txID,
		Origin:         domain.OriginExternal,
		ProviderID:     "provider-a",
		ExternalID:     "transaction-123",
		IdempotencyKey: "provider-a:transaction-123",
		PayloadHash:    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		WalletID:       walletID,
		PlayerID:       playerID,
		RoundID:        "round-987",
		GameID:         "fortune-chimp",
		Kind:           domain.KindBet,
		Money:          money.MustParse("25.00", brl),
	}
}

func registeredEnvelope() *cqrs.Envelope {
	return &cqrs.Envelope{
		Event:      &events.WagerTransactionRegistered{TransactionID: txID, Kind: domain.KindBet},
		OccurredAt: at,
		StreamID:   txID.String(),
		Version:    1,
	}
}

func Test_decide(t *testing.T) {
	reference := "transaction-999"

	tests := []struct {
		name        string
		history     []*cqrs.Envelope
		cmd         Command
		checkEvents func(t *testing.T, got []cqrs.Event)
		wantErr     bool
		wantCode    xerr.Code
	}{
		{
			name: "spec: Register Wager Operation - accepts a bet, moving nothing",
			cmd:  command(),
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				registered, ok := got[0].(*events.WagerTransactionRegistered)
				if !ok {
					t.Fatalf("event = %T, want *events.WagerTransactionRegistered", got[0])
				}
				if registered.TransactionID != txID {
					t.Errorf("transactionId = %s, want %s", registered.TransactionID, txID)
				}
				if registered.IdempotencyKey != "provider-a:transaction-123" {
					t.Errorf("idempotencyKey = %s", registered.IdempotencyKey)
				}
				if registered.PayloadHash == "" {
					t.Error("the accepted operation must record the hash of its business payload")
				}
				if !registered.Money.Equal(money.MustParse("25.00", brl)) {
					t.Errorf("money = %s, want 25.00", registered.Money)
				}
				if len(got) != 1 {
					t.Errorf("got %d events, want 1: acceptance moves no money", len(got))
				}
			},
		},
		{
			name: "spec: Register Wager Operation - accepts a LOSS that carries no amount",
			cmd: func() Command {
				c := command()
				c.ExternalID = "transaction-loss"
				c.IdempotencyKey = "provider-a:transaction-loss"
				c.Kind = domain.KindLoss
				c.Money = money.MustParse("0.00", brl)
				return c
			}(),
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				if len(got) != 1 {
					t.Fatalf("got %d events, want 1", len(got))
				}
			},
		},
		{
			name: "spec: Register Wager Operation - accepts a reversal that names its reference",
			cmd: func() Command {
				c := command()
				c.ExternalID = "transaction-rollback"
				c.IdempotencyKey = "provider-a:transaction-rollback"
				c.Kind = domain.KindRollback
				c.ReferenceExternalID = &reference
				return c
			}(),
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				registered := got[0].(*events.WagerTransactionRegistered)
				if registered.ReferenceExtern == nil || *registered.ReferenceExtern != reference {
					t.Errorf("reference = %v, want %s", registered.ReferenceExtern, reference)
				}
			},
		},
		{
			name: "spec: Register Wager Operation - refuses a reversal without a reference",
			cmd: func() Command {
				c := command()
				c.Kind = domain.KindRefund
				return c
			}(),
			wantErr:  true,
			wantCode: xerr.CodeReferenceRequired,
		},
		{
			name: "spec: Register Wager Operation - refuses a LOSS carrying an amount",
			cmd: func() Command {
				c := command()
				c.Kind = domain.KindLoss
				return c
			}(),
			wantErr:  true,
			wantCode: xerr.CodeLossAmountMustBeZero,
		},
		{
			name: "spec: Register Wager Operation - refuses a bet with no amount",
			cmd: func() Command {
				c := command()
				c.Money = money.MustParse("0.00", brl)
				return c
			}(),
			wantErr:  true,
			wantCode: xerr.CodeAmountMustBePositive,
		},
		{
			name: "spec: Register Wager Operation - refuses an operation with no idempotency key",
			cmd: func() Command {
				c := command()
				c.IdempotencyKey = ""
				return c
			}(),
			wantErr:  true,
			wantCode: xerr.CodeInvalidRequest,
		},
		{
			name: "spec: Register Wager Operation - refuses to accept the same transaction twice",
			cmd:  command(),
			history: []*cqrs.Envelope{
				registeredEnvelope(),
			},
			wantErr:  true,
			wantCode: xerr.CodeExternalTransactionConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := initialState()
			for _, e := range tt.history {
				s = evolve(s, e)
			}

			got, err := decide(s, tt.cmd)
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

func Test_evolve_tracksTheLifecycle(t *testing.T) {
	tests := []struct {
		name  string
		event cqrs.Event
		want  domain.Status
	}{
		{"registered", &events.WagerTransactionRegistered{TransactionID: txID}, domain.StatusPending},
		{"waiting for a reference", &events.WagerTransactionPendingReference{TransactionID: txID}, domain.StatusPendingReference},
		{"processed", &events.WagerTransactionProcessed{TransactionID: txID}, domain.StatusProcessed},
		{"rejected", &events.WagerTransactionRejected{TransactionID: txID}, domain.StatusRejected},
		{"failed", &events.WagerTransactionFailed{TransactionID: txID}, domain.StatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := evolve(initialState(), &cqrs.Envelope{Event: tt.event, OccurredAt: at})
			if s.status != tt.want {
				t.Errorf("status = %s, want %s", s.status, tt.want)
			}
			if !s.registered {
				t.Error("state does not know the transaction was accepted")
			}
		})
	}
}
