package events

import (
	"github.com/ironledger/iron-ledger/internal/platform/integration"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
)

// Builder publishes the wallet events that are part of the external contract.
//
// WalletOpened stays internal: it records that a wallet exists, which is a fact
// about this service's own storage, not about a player's money. WalletBalanceChanged
// is the financial event, and it is published with exactly the fields its
// contract promises.
type Builder struct{}

// NewBuilder returns the wallet integration builder.
func NewBuilder() Builder { return Builder{} }

// Build implements integration.Builder.
func (Builder) Build(source integration.Source) (integration.Envelope, bool) {
	switch event := source.Event.(type) {
	case *WalletBalanceChanged:
		return integration.Envelope{
			EventID:       source.EventID,
			EventType:     integration.EventWalletBalanceChanged,
			AggregateType: integration.AggregateWallet,
			AggregateID:   event.WalletID.String(),
			CorrelationID: integration.MetadataString(source.Metadata, integration.MetaCorrelationID),
			CausationID:   integration.MetadataString(source.Metadata, integration.MetaCausationID),
			OccurredAt:    source.OccurredAt,
			Version:       integration.Version,
			Data:          balanceChangedData(event),
		}, true
	default:
		return integration.Envelope{}, false
	}
}

// MoneyPayload is the wire form of a monetary value.
type MoneyPayload struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// BalanceChangedData is the published payload of WalletBalanceChanged.
type BalanceChangedData struct {
	WalletID      string       `json:"walletId"`
	TransactionID string       `json:"transactionId"`
	Direction     string       `json:"direction"`
	Money         MoneyPayload `json:"money"`
	BalanceBefore MoneyPayload `json:"balanceBefore"`
	BalanceAfter  MoneyPayload `json:"balanceAfter"`
	WalletVersion int64        `json:"walletVersion"`
}

func moneyPayload(m money.Money) MoneyPayload {
	return MoneyPayload{Amount: m.String(), Currency: string(m.Currency())}
}

func balanceChangedData(event *WalletBalanceChanged) BalanceChangedData {
	return BalanceChangedData{
		WalletID:      event.WalletID.String(),
		TransactionID: event.TransactionID.String(),
		Direction:     string(event.Direction),
		Money:         moneyPayload(event.Money),
		BalanceBefore: moneyPayload(event.BalanceBefore),
		BalanceAfter:  moneyPayload(event.BalanceAfter),
		WalletVersion: event.WalletVersion,
	}
}
