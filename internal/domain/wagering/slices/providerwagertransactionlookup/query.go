// Package providerwagertransactionlookup is the STATE_VIEW slice behind the
// provider-facing read.
//
// A provider sees its own transactions and nothing else. The provider identity
// is part of the lookup itself rather than a filter applied afterwards, so a
// cross-provider request cannot be answered even by accident — there is no code
// path that fetches "the" transaction and then decides whether to reveal it.
package providerwagertransactionlookup

import (
	"context"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/wagertransactiondetails"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// Reader resolves transactions by the provider's own identity.
type Reader interface {
	FindByExternalID(ctx context.Context, providerID, externalID string) (*wagertransactiondetails.Entity, error)
}

// Query looks a transaction up by provider and external identity.
type Query struct {
	ProviderID string
	ExternalID string
}

// ID implements cqrs.Query.
func (q Query) ID() []byte {
	return append([]byte("providerwagertransactionlookup:"+q.ProviderID+":"), []byte(q.ExternalID)...)
}

// ReadModel is the result of a provider lookup.
type ReadModel struct {
	Data *wagertransactiondetails.Entity
}

// QueryHandler answers provider-facing lookups.
type QueryHandler struct {
	reader Reader
}

var _ cqrs.QueryHandler[Query, *ReadModel] = (*QueryHandler)(nil)

// NewQueryHandler builds the handler.
func NewQueryHandler(reader Reader) *QueryHandler { return &QueryHandler{reader: reader} }

// HandleQuery returns the transaction, scoped to the provider.
func (h *QueryHandler) HandleQuery(ctx context.Context, qry Query) (*ReadModel, error) {
	if qry.ProviderID == "" || qry.ExternalID == "" {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Incomplete lookup",
			"Both $providerId and $externalTransactionId are required.",
			map[string]any{"providerId": qry.ProviderID, "externalTransactionId": qry.ExternalID})
	}
	entity, err := h.reader.FindByExternalID(ctx, qry.ProviderID, qry.ExternalID)
	if err != nil {
		if e, ok := xerr.As(err); ok {
			return nil, e
		}
		return nil, xerr.NotFound(xerr.CodeResourceNotFound, "Transaction not found",
			"Transaction $external does not exist for this provider.",
			map[string]any{"external": qry.ExternalID})
	}
	return &ReadModel{Data: entity}, nil
}
