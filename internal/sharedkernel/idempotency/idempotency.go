// Package idempotency computes the hash a request is deduplicated by.
//
// The hash covers only business fields, never transport metadata, and it is
// computed over a canonical JSON encoding with keys sorted at every level. Two
// requests that mean the same thing therefore hash the same whether they
// arrived over HTTP or over SQS, and whether the provider wrote "25" or
// "25.00" — the normalisation happens here, before the hash, so equivalent
// spellings converge instead of being treated as a payload conflict.
package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
)

// Payload is the business content of a wager operation, and exactly what the
// hash covers.
//
// The idempotency key is deliberately absent: it identifies the request, it is
// not part of what the request says. Nor is the message id, the correlation id,
// the HTTP method or any other transport artefact.
type Payload struct {
	ProviderID            string `json:"providerId"`
	ExternalTransactionID string `json:"externalTransactionId"`
	PlayerID              string `json:"playerId"`
	WalletID              string `json:"walletId"`
	RoundID               string `json:"roundId"`
	GameID                string `json:"gameId"`
	Kind                  string `json:"kind"`
	Amount                string `json:"amount"`
	Currency              string `json:"currency"`
	ReferenceExternalID   string `json:"referenceExternalTransactionId,omitempty"`
}

// Hash returns the lowercase hex SHA-256 of the canonical encoding.
//
// encoding/json sorts object keys, so marshalling a struct already produces the
// canonical form: no insignificant whitespace, keys in lexicographic order at
// every depth, and monetary values as fixed-scale decimal strings.
func Hash(payload Payload) (string, error) {
	canonical, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("idempotency: encode payload: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// OfMoney builds a payload from already-parsed values, normalising the amount
// to its two-decimal form.
func OfMoney(providerID, externalID string, playerID, walletID uuid.UUID, roundID, gameID, kind string, amount money.Money, reference *string) Payload {
	payload := Payload{
		ProviderID:            providerID,
		ExternalTransactionID: externalID,
		PlayerID:              playerID.String(),
		WalletID:              walletID.String(),
		RoundID:               roundID,
		GameID:                gameID,
		Kind:                  kind,
		Amount:                amount.String(),
		Currency:              string(amount.Currency()),
	}
	if reference != nil {
		payload.ReferenceExternalID = *reference
	}
	return payload
}
