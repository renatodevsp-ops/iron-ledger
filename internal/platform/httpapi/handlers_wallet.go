package httpapi

import (
	"net/http"

	"github.com/ironledger/ironledger/internal/domain"
)

// walletResponse is the Wallet schema from the contract.
type walletResponse struct {
	ID           string              `json:"id"`
	PlayerID     string              `json:"playerId,omitempty"`
	Currency     domain.Currency     `json:"currency"`
	BalanceMinor int64               `json:"balanceMinor"`
	Status       domain.WalletStatus `json:"status"`
	Version      int64               `json:"version"`
	UpdatedAt    string              `json:"updatedAt"`
}

// getWallet implements GET /v1/wallets/{walletId}.
func (s *Server) getWallet(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, domain.ErrUnauthorized)
		return
	}
	walletID := r.PathValue("walletId")
	if !isUUID(walletID) {
		writeError(w, domain.NewError(domain.ReasonWalletNotFound, "no such wallet").
			WithField("walletId"))
		return
	}

	view, err := s.svc.GetBalance(r.Context(), principal.TenantID, walletID)
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, walletResponse{
		ID:           view.WalletID,
		PlayerID:     view.PlayerID,
		Currency:     view.Currency,
		BalanceMinor: view.BalanceMinor,
		Status:       view.Status,
		Version:      view.Version,
		UpdatedAt:    view.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	})
}

// liveness answers /health/live. It must not touch a dependency: a liveness
// probe that fails when the database is down would make the orchestrator
// restart a process that is perfectly able to recover on its own.
func (s *Server) liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readiness answers /health/ready. Unlike liveness it does check the database:
// a pod that cannot reach PostgreSQL cannot serve traffic and should be taken
// out of rotation.
func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	checks := map[string]string{}
	ready := true

	if err := s.health(ctx); err != nil {
		checks["database"] = err.Error()
		ready = false
	} else {
		checks["database"] = "ok"
	}

	status := http.StatusOK
	state := "ok"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "unavailable"
	}
	writeJSON(w, status, map[string]any{"status": state, "checks": checks})
}
