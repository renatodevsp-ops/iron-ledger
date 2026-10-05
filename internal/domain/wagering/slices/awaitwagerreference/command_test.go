package awaitwagerreference

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
	walletID     = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	playerID     = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	brl          = money.Currency("BRL")
	at           = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	referenceRef = "transaction-999"
	nextAttempt  = at.Add(30 * time.Second)
)

func registeredEvent(withReference bool) *events.WagerTransactionRegistered {
	registered := &events.WagerTransactionRegistered{
		TransactionID:  txID,
		Origin:         domain.OriginExternal,
		ProviderID:     "provider-a",
		ExternalID:     "transaction-rollback",
		IdempotencyKey: "provider-a:transaction-rollback",
		WalletID:       walletID,
		PlayerID:       playerID,
		RoundID:        "round-987",
		GameID:         "fortune-chimp",
		Kind:           domain.KindRollback,
		Money:          money.MustParse("25.00", brl),
	}
	if withReference {
		registered.ReferenceExtern = &referenceRef
	}
	return registered
}

func envelope(event cqrs.Event, version uint64) *cqrs.Envelope {
	return &cqrs.Envelope{Event: event, OccurredAt: at, StreamID: txID.String(), Version: version}
}

func Test_decide(t *testing.T) {
	tests := []struct {
		name        string
		history     []*cqrs.Envelope
		cmd         Command
		checkEvents func(t *testing.T, got []cqrs.Event)
		wantErr     bool
		wantCode    xerr.Code
	}{
		{
			name:    "spec: Await Wager Reference - parks a reversal and schedules the next attempt",
			history: []*cqrs.Envelope{envelope(registeredEvent(true), 1)},
			cmd:     Command{TransactionID: txID, NextAttemptAt: nextAttempt, Reason: "not received yet"},
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				waiting, ok := got[0].(*events.WagerTransactionPendingReference)
				if !ok {
					t.Fatalf("event = %T, want *events.WagerTransactionPendingReference", got[0])
				}
				if waiting.ReferenceExtern != referenceRef {
					t.Errorf("reference = %s, want %s", waiting.ReferenceExtern, referenceRef)
				}
				if waiting.Attempt != 1 {
					t.Errorf("attempt = %d, want 1", waiting.Attempt)
				}
				if !waiting.NextAttemptAt.Equal(nextAttempt) {
					t.Errorf("nextAttemptAt = %s, want %s", waiting.NextAttemptAt, nextAttempt)
				}
				if waiting.Reason == "" {
					t.Error("the wait must record why it is waiting")
				}
			},
		},
		{
			name: "spec: Await Wager Reference - accumulates attempts on every reschedule",
			history: []*cqrs.Envelope{
				envelope(registeredEvent(true), 1),
				envelope(&events.WagerTransactionPendingReference{
					TransactionID:   txID,
					ReferenceExtern: referenceRef,
					Attempt:         3,
					NextAttemptAt:   at.Add(time.Second),
				}, 2),
			},
			cmd: Command{TransactionID: txID, NextAttemptAt: nextAttempt, Reason: "still not received"},
			checkEvents: func(t *testing.T, got []cqrs.Event) {
				waiting := got[0].(*events.WagerTransactionPendingReference)
				if waiting.Attempt != 4 {
					t.Errorf("attempt = %d, want 4", waiting.Attempt)
				}
			},
		},
		{
			name:     "spec: Await Wager Reference - refuses an operation that names no reference",
			history:  []*cqrs.Envelope{envelope(registeredEvent(false), 1)},
			cmd:      Command{TransactionID: txID, NextAttemptAt: nextAttempt, Reason: "why"},
			wantErr:  true,
			wantCode: xerr.CodeReferenceRequired,
		},
		{
			name:     "spec: Await Wager Reference - refuses a transaction that was never accepted",
			history:  nil,
			cmd:      Command{TransactionID: txID, NextAttemptAt: nextAttempt, Reason: "why"},
			wantErr:  true,
			wantCode: xerr.CodeResourceNotFound,
		},
		{
			name: "spec: Await Wager Reference - refuses to park a transaction that already succeeded",
			history: []*cqrs.Envelope{
				envelope(registeredEvent(true), 1),
				envelope(&events.WagerTransactionProcessed{
					TransactionID: txID,
					Kind:          domain.KindRollback,
					BalanceAfter:  money.MustParse("25.00", brl),
				}, 2),
			},
			cmd:     Command{TransactionID: txID, NextAttemptAt: nextAttempt, Reason: "why"},
			wantErr: true,
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
