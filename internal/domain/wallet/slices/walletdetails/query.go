package walletdetails

import (
	"context"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// QueryHandler answers wallet lookups from the projection.
type QueryHandler struct {
	repo WalletDetailsRepository
}

var _ cqrs.QueryHandler[WalletDetailsQuery, *WalletDetailsReadModel] = (*QueryHandler)(nil)

// NewQueryHandler builds the handler.
func NewQueryHandler(repo WalletDetailsRepository) *QueryHandler {
	return &QueryHandler{repo: repo}
}

// HandleQuery returns a wallet by id.
func (h *QueryHandler) HandleQuery(ctx context.Context, qry WalletDetailsQuery) (*WalletDetailsReadModel, error) {
	entity, err := h.repo.Find(ctx, qry.WalletID.String())
	if err != nil {
		return nil, translate(err, qry.WalletID.String())
	}
	return &WalletDetailsReadModel{Data: entity}, nil
}

// HandleQueryByPlayer returns a wallet by owner and currency.
func (h *QueryHandler) HandleQueryByPlayer(ctx context.Context, qry WalletByPlayerQuery) (*WalletDetailsReadModel, error) {
	entity, err := h.repo.FindByPlayer(ctx, qry.PlayerID, qry.Currency)
	if err != nil {
		return nil, translate(err, "")
	}
	return &WalletDetailsReadModel{Data: entity}, nil
}

func translate(err error, walletID string) error {
	if e, ok := xerr.As(err); ok {
		return e
	}
	return xerr.NotFound(xerr.CodeResourceNotFound, "Wallet not found",
		"Wallet $wallet does not exist.",
		map[string]any{"wallet": walletID})
}
