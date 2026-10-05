package settlewageroperation

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
	txID         = uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d")
	referenceTx  = uuid.MustParse("0192f297-345e-7e38-af88-e43f851a819d")
	walletID     = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	playerID     = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	brl          = money.Currency("BRL")
	at           = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	externalID   = "transaction-123"
	referenceRef = "transaction-999"
)

func history(events ...cqrs.Event) []*cqrs.Envelope {
	out := make([]*cqrs.Envelope, 0, len(events))
	for i, event := range events {
		out = append(out, &cqrs.Envelope{
			Event:      event,
			OccurredAt: at,
			StreamID:   txID.String(),
			Version:    uint64(i + 1),
		})
	}
	return out
}

func registered(kind domain.Kind, amount money.Money) *events.WagerTransactionRegistered {
	external := externalID
	if kind.RequiresReference() {
		external = "transaction-rollback"
	}
	registered := &events.WagerTransactionRegistered{
		TransactionID:  txID,
		Origin:         domain.OriginExternal,
		ProviderID:     "provider-a",
		ExternalID:     external,
		IdempotencyKey: "provider-a:" + external,
		WalletID:       walletID,
		PlayerID:       playerID,
		RoundID:        "round-987",
		GameID:         "fortune-chimp",
		Kind:           kind,
		Money:          amount,
	}
	if kind.RequiresReference() {
		registered.ReferenceExtern = &referenceRef
	}
	return registered
}

func Test_decide_settles(t *testing.T) {
	tests := []struct {
		name        string
		history     []*cqrs.Envelope
		cmd         Command
		checkEvents func(t *testing.T, got []cqrs.Event)
		wantErr     bool
		wantCode    xerr.Code
	}{
		{
			name:    "spec: Settle Wager Operation - records a successful settlement with the balance observed",
			history: history(registered(domain.KindBet, money.MustParse("25.00", brl))),
			cmd: Command{
				TransactionID: txID,
				Outcome:       Outcome{Processed: true, BalanceAfter: money.MustParse("75.00", brl)},
			},
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				processed, ok := got[0].(*events.WagerTransactionProcessed)
				if !ok {
					t.Fatalf("event = %T, want *events.WagerTransactionProcessed", got[0])
				}
				if !processed.BalanceAfter.Equal(money.MustParse("75.00", brl)) {
					t.Errorf("balanceAfter = %s, want 75.00", processed.BalanceAfter)
				}
				if processed.Kind != domain.KindBet {
					t.Errorf("kind = %s, want BET", processed.Kind)
				}
				if processed.ProviderID != "provider-a" {
					t.Errorf("providerId = %s", processed.ProviderID)
				}
			},
		},
		{
			name: "spec: Settle Wager Operation - records the resolved reference of a reversal",
			history: history(
				registered(domain.KindRollback, money.MustParse("25.00", brl)),
				&events.WagerTransactionPendingReference{
					TransactionID:   txID,
					ReferenceExtern: referenceRef,
					Attempt:         1,
					NextAttemptAt:   at.Add(time.Minute),
				},
			),
			cmd: Command{
				TransactionID: txID,
				Outcome: Outcome{
					Processed:         true,
					BalanceAfter:      money.MustParse("75.00", brl),
					ResolvedReference: &referenceTx,
				},
			},
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				processed := got[0].(*events.WagerTransactionProcessed)
				if processed.ResolvedReference == nil || *processed.ResolvedReference != referenceTx {
					t.Errorf("resolvedReference = %v, want %s", processed.ResolvedReference, referenceTx)
				}
			},
		},
		{
			name:    "spec: Settle Wager Operation - settles a LOSS that moved no money",
			history: history(registered(domain.KindLoss, money.MustParse("0.00", brl))),
			cmd: Command{
				TransactionID: txID,
				Outcome:       Outcome{Processed: true, BalanceAfter: money.MustParse("75.00", brl)},
			},
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				if got[0].EventType() != "WagerTransactionProcessed" {
					t.Errorf("eventType = %s, want WagerTransactionProcessed", got[0].EventType())
				}
			},
		},
		{
			name:    "spec: Settle Wager Operation - records a terminal rejection with a stable code",
			history: history(registered(domain.KindBet, money.MustParse("80.00", brl))),
			cmd: Command{
				TransactionID: txID,
				Outcome: Outcome{
					FailureCode:    xerr.CodeInsufficientBalance,
					FailureMessage: "The wallet holds 100.00, which does not cover 80.00.",
				},
			},
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				rejected, ok := got[0].(*events.WagerTransactionRejected)
				if !ok {
					t.Fatalf("event = %T, want *events.WagerTransactionRejected", got[0])
				}
				if rejected.FailureCode != xerr.CodeInsufficientBalance {
					t.Errorf("failureCode = %s", rejected.FailureCode)
				}
				if rejected.FailureMessage == "" {
					t.Error("a rejection must explain itself")
				}
			},
		},
		{
			name:    "spec: Settle Wager Operation - refuses to settle a transaction that was never accepted",
			history: nil,
			cmd: Command{
				TransactionID: txID,
				Outcome:       Outcome{Processed: true, BalanceAfter: money.MustParse("75.00", brl)},
			},
			wantErr:  true,
			wantCode: xerr.CodeResourceNotFound,
		},
		{
			name: "spec: Settle Wager Operation - refuses to settle a terminal transaction again",
			history: history(
				registered(domain.KindBet, money.MustParse("25.00", brl)),
				&events.WagerTransactionProcessed{
					TransactionID: txID,
					Kind:          domain.KindBet,
					BalanceAfter:  money.MustParse("75.00", brl),
				},
			),
			cmd: Command{
				TransactionID: txID,
				Outcome:       Outcome{Processed: true, BalanceAfter: money.MustParse("50.00", brl)},
			},
			wantErr:  true,
			wantCode: xerr.CodeExternalTransactionConflict,
		},
		{
			name:    "spec: Settle Wager Operation - refuses a rejection with no code",
			history: history(registered(domain.KindBet, money.MustParse("25.00", brl))),
			cmd:     Command{TransactionID: txID, Outcome: Outcome{}},
			wantErr: true,
		},
		{
			name:    "spec: Settle Wager Operation - refuses a settlement whose balance is in another currency",
			history: history(registered(domain.KindBet, money.MustParse("25.00", brl))),
			cmd: Command{
				TransactionID: txID,
				Outcome:       Outcome{Processed: true, BalanceAfter: money.MustParse("75.00", "USD")},
			},
			wantErr:  true,
			wantCode: xerr.CodeCurrencyMismatch,
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
				if tt.wantCode != "" {
					if code, _ := xerr.CodeOf(err); code != tt.wantCode {
						t.Errorf("error code = %v, want %v", code, tt.wantCode)
					}
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
