package wagertransactiondetails

import (
	"context"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// QueryHandler answers transaction lookups by internal identity.
type QueryHandler struct {
	repo Repository
}

var _ cqrs.QueryHandler[Query, *ReadModel] = (*QueryHandler)(nil)

// NewQueryHandler builds the handler.
func NewQueryHandler(repo Repository) *QueryHandler { return &QueryHandler{repo: repo} }

// HandleQuery returns a transaction by id, including its failure code when it
// was rejected.
func (h *QueryHandler) HandleQuery(ctx context.Context, qry Query) (*ReadModel, error) {
	if qry.TransactionID.String() == "" {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Missing transaction",
			"$field is required.", map[string]any{"field": "transactionId"})
	}
	entity, err := h.repo.Find(ctx, qry.TransactionID.String())
	if err != nil {
		if e, ok := xerr.As(err); ok {
			return nil, e
		}
		return nil, xerr.NotFound(xerr.CodeResourceNotFound, "Transaction not found",
			"Transaction $transaction does not exist.",
			map[string]any{"transaction": qry.TransactionID.String()})
	}
	return &ReadModel{Data: entity}, nil
}
