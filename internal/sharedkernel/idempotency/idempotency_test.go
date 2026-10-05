package idempotency

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
)

var (
	playerID = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	walletID = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	brl      = money.Currency("BRL")
)

func payload(amount string) Payload {
	return OfMoney("provider-a", "transaction-123", playerID, walletID,
		"round-987", "fortune-chimp", "BET", money.MustParse(amount, brl), nil)
}

func TestHash_isStableAcrossEquivalentAmounts(t *testing.T) {
	// The normalisation happens when the amount is parsed, before the hash, so
	// the three spellings of twenty-five are one request rather than three.
	want, err := Hash(payload("25.00"))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	for _, amount := range []string{"25", "25.0", "25.00"} {
		got, err := Hash(payload(amount))
		if err != nil {
			t.Fatalf("Hash(%q): %v", amount, err)
		}
		if got != want {
			t.Errorf("Hash(%q) = %s, want %s", amount, got, want)
		}
	}
}

func TestHash_changesWithAnyBusinessField(t *testing.T) {
	base, err := Hash(payload("25.00"))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	reference := "transaction-999"
	variants := map[string]Payload{
		"different amount":      OfMoney("provider-a", "transaction-123", playerID, walletID, "round-987", "fortune-chimp", "BET", money.MustParse("26.00", brl), nil),
		"different external id": OfMoney("provider-a", "transaction-124", playerID, walletID, "round-987", "fortune-chimp", "BET", money.MustParse("25.00", brl), nil),
		"different provider":    OfMoney("provider-b", "transaction-123", playerID, walletID, "round-987", "fortune-chimp", "BET", money.MustParse("25.00", brl), nil),
		"different wallet":      OfMoney("provider-a", "transaction-123", playerID, uuid.New(), "round-987", "fortune-chimp", "BET", money.MustParse("25.00", brl), nil),
		"different round":       OfMoney("provider-a", "transaction-123", playerID, walletID, "round-988", "fortune-chimp", "BET", money.MustParse("25.00", brl), nil),
		"different game":        OfMoney("provider-a", "transaction-123", playerID, walletID, "round-987", "fortune-tiger", "BET", money.MustParse("25.00", brl), nil),
		"different kind":        OfMoney("provider-a", "transaction-123", playerID, walletID, "round-987", "fortune-chimp", "WIN", money.MustParse("25.00", brl), nil),
		"with a reference":      OfMoney("provider-a", "transaction-123", playerID, walletID, "round-987", "fortune-chimp", "ROLLBACK", money.MustParse("25.00", brl), &reference),
	}

	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			got, err := Hash(variant)
			if err != nil {
				t.Fatalf("Hash: %v", err)
			}
			if got == base {
				t.Error("a different business payload produced the same hash")
			}
		})
	}
}

// Hash2 is a small readability helper around Hash.
func Hash2(t *testing.T, payload Payload) string {
	t.Helper()
	got, err := Hash(payload)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	return got
}

func TestHash_isHexSHA256(t *testing.T) {
	got := Hash2(t, payload("25.00"))
	if len(got) != 64 {
		t.Fatalf("hash = %q, want 64 hex characters", got)
	}
	for _, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("hash = %q, want lowercase hex", got)
		}
	}
}
