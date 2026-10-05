package events

import (
	"github.com/google/uuid"

	walleftevents "github.com/ironledger/iron-ledger/internal/domain/wallet/events"
	"github.com/ironledger/iron-ledger/internal/platform/integration"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
)

// MoneyPayload is the wire form of a monetary value.
type MoneyPayload = walleftevents.MoneyPayload

// OperationPayload is the shape shared by the operation events: enough to
// identify the transaction and the money involved, and nothing more. It
// deliberately carries no balance, no player identity beyond the id, and no
// credentials.
type OperationPayload struct {
	TransactionID         string       `json:"transactionId"`
	ProviderID            string       `json:"providerId,omitempty"`
	ExternalTransactionID string       `json:"externalTransactionId,omitempty"`
	WalletID              string       `json:"walletId"`
	PlayerID              string       `json:"playerId,omitempty"`
	RoundID               string       `json:"roundId,omitempty"`
	GameID                string       `json:"gameId,omitempty"`
	Kind                  string       `json:"kind"`
	Money                 MoneyPayload `json:"money"`
}

// ProcessedData is the published payload of WagerTransactionProcessed.
type ProcessedData struct {
	OperationPayload
	BalanceAfter           MoneyPayload `json:"balanceAfter"`
	ReferenceTransactionID string       `json:"referenceTransactionId,omitempty"`
}

// RejectedData is the published payload of WagerTransactionRejected.
type RejectedData struct {
	OperationPayload
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

// PendingReferenceData is the published payload of
// WagerTransactionPendingReference.
type PendingReferenceData struct {
	OperationPayload
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId"`
	Attempt                        int    `json:"attempt"`
	NextAttemptAt                  string `json:"nextAttemptAt"`
	Reason                         string `json:"reason"`
}

// Builder publishes the wagering events that are part of the external
// contract. WagerTransactionRegistered and WagerTransactionFailed stay
// internal: they are bookkeeping about our own attempts, not facts about a
// player's money.
type Builder struct{}

// NewBuilder returns the wagering integration builder.
func NewBuilder() Builder { return Builder{} }

// Build implements integration.Builder.
func (Builder) Build(source integration.Source) (integration.Envelope, bool) {
	base := integration.Envelope{
		EventID:       source.EventID,
		AggregateType: integration.AggregateWagerTransaction,
		AggregateID:   source.AggregateID,
		CorrelationID: integration.MetadataString(source.Metadata, integration.MetaCorrelationID),
		CausationID:   integration.MetadataString(source.Metadata, integration.MetaCausationID),
		OccurredAt:    source.OccurredAt,
		Version:       integration.Version,
	}

	switch event := source.Event.(type) {
	case *WagerTransactionProcessed:
		base.EventType = integration.EventWagerTransactionProcessed
		base.Data = ProcessedData{
			OperationPayload:       payload(event.TransactionID, event.ProviderID, event.ExternalID, event.WalletID, event.PlayerID, event.RoundID, event.GameID, string(event.Kind), event.Money),
			BalanceAfter:           moneyPayload(event.BalanceAfter),
			ReferenceTransactionID: uuidOrEmpty(event.ResolvedReference),
		}
		return base, true

	case *WagerTransactionRejected:
		base.EventType = integration.EventWagerTransactionRejected
		base.Data = RejectedData{
			OperationPayload: payload(event.TransactionID, event.ProviderID, event.ExternalID, event.WalletID, event.PlayerID, event.RoundID, event.GameID, string(event.Kind), event.Money),
			FailureCode:      string(event.FailureCode),
			FailureMessage:   event.FailureMessage,
		}
		return base, true

	case *WagerTransactionPendingReference:
		base.EventType = integration.EventWagerTransactionPendingRef
		base.Data = PendingReferenceData{
			OperationPayload:               payload(event.TransactionID, event.ProviderID, event.ExternalID, event.WalletID, event.PlayerID, event.RoundID, event.GameID, string(event.Kind), event.Money),
			ReferenceExternalTransactionID: event.ReferenceExtern,
			Attempt:                        event.Attempt,
			NextAttemptAt:                  event.NextAttemptAt.UTC().Format(rfc3339Millis),
			Reason:                         event.Reason,
		}
		return base, true

	default:
		return integration.Envelope{}, false
	}
}

// rfc3339Millis is the timestamp layout every published event uses: UTC, with
// millisecond precision.
const rfc3339Millis = "2006-01-02T15:04:05.000Z07:00"

func payload(transactionID uuid.UUID, providerID, externalID string, walletID, playerID uuid.UUID,
	roundID, gameID, kind string, amount money.Money,
) OperationPayload {
	return OperationPayload{
		TransactionID:         transactionID.String(),
		ProviderID:            providerID,
		ExternalTransactionID: externalID,
		WalletID:              walletID.String(),
		PlayerID:              playerID.String(),
		RoundID:               roundID,
		GameID:                gameID,
		Kind:                  kind,
		Money:                 moneyPayload(amount),
	}
}

func moneyPayload(m money.Money) MoneyPayload {
	return MoneyPayload{Amount: m.String(), Currency: string(m.Currency())}
}

func uuidOrEmpty(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}
