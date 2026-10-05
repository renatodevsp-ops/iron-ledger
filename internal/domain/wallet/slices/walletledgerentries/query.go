package walletledgerentries

import (
	"context"
	"fmt"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/platform/repository"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// QueryHandler pages the ledger of one wallet.
type QueryHandler struct {
	repo Repository
}

var _ cqrs.QueryHandler[EntryQuery, *EntryReadModel] = (*QueryHandler)(nil)

// NewQueryHandler builds the handler.
func NewQueryHandler(repo Repository) *QueryHandler { return &QueryHandler{repo: repo} }

// HandleQuery returns one page of ledger entries, oldest first.
func (h *QueryHandler) HandleQuery(ctx context.Context, qry EntryQuery) (*EntryReadModel, error) {
	if qry.WalletID.String() == "" {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Missing wallet",
			"$field is required.", map[string]any{"field": "walletId"})
	}

	after, err := repository.DecodePageCursor(qry.Cursor)
	if err != nil {
		return nil, xerr.Validation(xerr.CodeInvalidRequest, "Invalid cursor",
			"$cursor is not a cursor issued by this endpoint. Omit it to read the first page.",
			map[string]any{"cursor": qry.Cursor})
	}

	connection, err := h.repo.Page(ctx, qry.WalletID, after, qry.Limit)
	if err != nil {
		return nil, fmt.Errorf("walletledgerentries: %w", err)
	}
	return &EntryReadModel{Data: connection.Nodes, Cursor: connection.Cursor}, nil
}
