// Package httpapi is the HTTP surface of the platform.
//
// Handlers are thin on purpose: they translate the wire format into a use-case
// request, translate the result back, and render failures through one shared
// problem shape. No business rule lives here, and no money is ever parsed into
// a float — the amount crosses the boundary as the decimal string the provider
// sent, and the Money value object decides what it means.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ironledger/iron-ledger/internal/app/usecase"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/identity"
	"github.com/ironledger/iron-ledger/internal/platform/httpx"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// IdempotencyHeader is the header carrying the caller's deduplication key.
const IdempotencyHeader = "Idempotency-Key"

// Handler is the API surface.
type Handler struct {
	wallets     *usecase.WalletsUseCase
	wagering    *usecase.WageringUseCase
	ledger      *usecase.LedgerUseCase
	readQueries *usecase.Queries
	logger      *slog.Logger
}

// NewHandler builds the HTTP handler.
func NewHandler(
	wallets *usecase.WalletsUseCase,
	wagering *usecase.WageringUseCase,
	ledger *usecase.LedgerUseCase,
	queries *usecase.Queries,
	logger *slog.Logger,
) *Handler {
	return &Handler{wallets: wallets, wagering: wagering, ledger: ledger, readQueries: queries, logger: logger}
}

// Router builds the chi router with the middleware chain applied.
//
// Health and metrics are mounted outside the authentication chain on purpose:
// a probe must answer even when the identity provider is down, and it exposes
// nothing but liveness.
func (h *Handler) Router(authenticated func(http.Handler) http.Handler, health http.Handler, metricsHandler http.Handler, maxBody int64) http.Handler {
	r := chi.NewRouter()

	r.Method(http.MethodGet, "/health/live", health)
	r.Method(http.MethodGet, "/health/ready", health)
	r.Handle("/metrics", metricsHandler)

	r.Group(func(private chi.Router) {
		private.Use(authenticated)

		// Wallet operations are the platform's own: a provider may never open a
		// wallet or read a balance.
		private.Post("/wallets", h.openWallet)
		private.Get("/wallets/{walletId}", h.getWallet)
		private.Get("/wallets/{walletId}/ledger", h.listLedger)
		private.Post("/wallets/{walletId}/reconciliation", h.reconcileWallet)

		// Provider operations. The provider identity comes from the credential;
		// a providerId in the body is only cross-checked, never trusted.
		private.Post("/wagering/transactions", h.submitOperation)
		private.Get("/wagering/transactions/{transactionId}", h.getTransaction)
		private.Get("/providers/{providerId}/wagering/transactions/{externalTransactionId}", h.getProviderTransaction)
	})

	return httpx.Chain(r,
		// Correlation first, so every line below it — including the recovery of
		// a panic — carries the identifier a caller can quote.
		httpx.WithCorrelationID,
		httpx.Recoverer(h.logger),
		httpx.LimitBody(maxBody),
		httpx.WithObservability(h.logger, nil),
	)
}

// ── wallets ──────────────────────────────────────────────────────────────────

type moneyDTO struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type openWalletRequest struct {
	PlayerID       string    `json:"playerId"`
	InitialBalance *moneyDTO `json:"initialBalance"`
}

// openWallet opens a wallet.
//
// A positive opening balance creates the wallet, its OPENING transaction in
// PROCESSED, the credit ledger entry and both outbound events in one commit. A
// zero balance creates only the wallet. A second wallet for the same player and
// currency is a 409.
func (h *Handler) openWallet(w http.ResponseWriter, r *http.Request) {
	if _, err := identity.RequireInternal(r.Context()); err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}

	var body openWalletRequest
	if err := decodeJSON(r, &body); err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}

	playerID, err := parseUUID(body.PlayerID, "playerId")
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	if body.InitialBalance == nil {
		httpx.WriteProblem(w, r, h.logger, xerr.Validation(xerr.CodeInvalidRequest, "Missing initial balance",
			"$field is required.", map[string]any{"field": "initialBalance"}))
		return
	}
	initial, err := parseMoney(body.InitialBalance.Amount, body.InitialBalance.Currency, "initialBalance")
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}

	view, err := h.wallets.Open(r.Context(), usecase.OpenWalletRequest{
		PlayerID:      playerID,
		Initial:       initial,
		CorrelationID: r.Header.Get("X-Correlation-Id"),
	})
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, view)
}

// getWallet returns a wallet snapshot.
func (h *Handler) getWallet(w http.ResponseWriter, r *http.Request) {
	if _, err := identity.RequireInternal(r.Context()); err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}

	walletID, err := pathUUID(r, "walletId")
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	view, err := h.wallets.Get(r.Context(), walletID)
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// listLedger returns one page of the wallet's append-only ledger.
func (h *Handler) listLedger(w http.ResponseWriter, r *http.Request) {
	if _, err := identity.RequireInternal(r.Context()); err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}

	walletID, err := pathUUID(r, "walletId")
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	limit, err := queryInt(r, "limit")
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	page, err := h.ledger.List(r.Context(), walletID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

// reconcileWallet compares the stored balance with the ledger.
func (h *Handler) reconcileWallet(w http.ResponseWriter, r *http.Request) {
	if _, err := identity.RequireInternal(r.Context()); err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}

	walletID, err := pathUUID(r, "walletId")
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	report, err := h.ledger.Reconcile(r.Context(), walletID)
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, report)
}

// ── wagering ─────────────────────────────────────────────────────────────────

type submitOperationRequest struct {
	ProviderID            string    `json:"providerId"`
	ExternalTransactionID string    `json:"externalTransactionId"`
	PlayerID              string    `json:"playerId"`
	WalletID              string    `json:"walletId"`
	RoundID               string    `json:"roundId"`
	GameID                string    `json:"gameId"`
	Kind                  string    `json:"kind"`
	Money                 *moneyDTO `json:"money"`
	ReferenceExternalID   *string   `json:"referenceExternalTransactionId"`
}

// submitOperation reports one provider operation.
//
// The response distinguishes every outcome a provider must react to
// differently: 200 settled, 202 durably accepted and waiting for a reference,
// 409 a key or identity conflict, 422 a terminal business rejection carrying a
// stable code, 503 a transient dependency failure that is safe to retry with the
// same key.
func (h *Handler) submitOperation(w http.ResponseWriter, r *http.Request) {
	if _, err := identity.RequireProvider(r.Context()); err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}

	var body submitOperationRequest
	if err := decodeJSON(r, &body); err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}

	idempotencyKey := r.Header.Get(IdempotencyHeader)
	if idempotencyKey == "" {
		httpx.WriteProblem(w, r, h.logger, xerr.Validation(xerr.CodeInvalidRequest, "Missing idempotency key",
			"$header is required. Send {providerId}:{externalTransactionId} so a repeated delivery is recognised.",
			map[string]any{"header": IdempotencyHeader}))
		return
	}

	playerID, err := parseUUID(body.PlayerID, "playerId")
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	walletID, err := parseUUID(body.WalletID, "walletId")
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	if body.Money == nil {
		httpx.WriteProblem(w, r, h.logger, xerr.Validation(xerr.CodeInvalidRequest, "Missing amount",
			"$field is required.", map[string]any{"field": "money"}))
		return
	}

	// The provider identity is resolved by the authorizer from the credential
	// and compared with the body. The body never wins: a provider cannot submit
	// under another provider's identity.
	principal, err := h.providerFor(w, r, body.ProviderID)
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}

	result, err := h.wagering.Submit(r.Context(), usecase.OperationRequest{
		IdempotencyKey:        idempotencyKey,
		ProviderID:            principal.ProviderID,
		ExternalTransactionID: body.ExternalTransactionID,
		PlayerID:              playerID,
		WalletID:              walletID,
		RoundID:               body.RoundID,
		GameID:                body.GameID,
		Kind:                  body.Kind,
		Amount:                body.Money.Amount,
		Currency:              body.Money.Currency,
		ReferenceExternalID:   body.ReferenceExternalID,
		Source:                usecase.SourceHTTP,
	})
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}

	switch domain.Status(result.Status) {
	case domain.StatusProcessed:
		httpx.WriteJSON(w, http.StatusOK, result)
	case domain.StatusPendingReference, domain.StatusPending:
		httpx.WriteJSON(w, http.StatusAccepted, result)
	default:
		// A terminal rejection is a successful HTTP exchange carrying a
		// business decision, so the body is the result shape, not a problem.
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, result)
	}
}

// getTransaction returns a transaction by its internal identity.
func (h *Handler) getTransaction(w http.ResponseWriter, r *http.Request) {
	transactionID, err := pathUUID(r, "transactionId")
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	view, err := h.lookupTransaction(r, transactionID.String())
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// getProviderTransaction returns a transaction by the provider's own identity,
// scoped to the calling provider.
func (h *Handler) getProviderTransaction(w http.ResponseWriter, r *http.Request) {
	providerID := chi.URLParam(r, "providerId")
	externalID := chi.URLParam(r, "externalTransactionId")
	if providerID == "" || externalID == "" {
		httpx.WriteProblem(w, r, h.logger, xerr.Validation(xerr.CodeInvalidRequest, "Incomplete lookup",
			"Both $providerId and $externalTransactionId are required.",
			map[string]any{"providerId": providerID, "externalTransactionId": externalID}))
		return
	}
	view, err := h.lookupProviderTransaction(r, providerID, externalID)
	if err != nil {
		httpx.WriteProblem(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// ── decoding helpers ─────────────────────────────────────────────────────────

func decodeJSON(r *http.Request, target any) error {
	if r.Body == nil {
		return xerr.Validation(xerr.CodeInvalidRequest, "Missing body", "A JSON body is required.", nil)
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return xerr.Validation(xerr.CodeInvalidRequest, "Payload too large",
				"The request body is larger than this endpoint accepts.", nil)
		}
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return xerr.Validation(xerr.CodeInvalidRequest, "Malformed JSON",
				"The body is not valid JSON: $detail.", map[string]any{"detail": syntax.Error()})
		}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return xerr.Validation(xerr.CodeInvalidRequest, "Wrong field type",
				"Field $field must be of type $expected.",
				map[string]any{"field": typeErr.Field, "expected": typeErr.Type.String()})
		}
		return xerr.Validation(xerr.CodeInvalidRequest, "Unreadable body",
			"The body could not be read: $detail.", map[string]any{"detail": err.Error()})
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return xerr.Validation(xerr.CodeInvalidRequest, "Trailing content",
			"The body must contain exactly one JSON object.", nil)
	}
	return nil
}

func parseMoney(amount, currency, field string) (money.Money, error) {
	parsed, err := money.Parse(amount, money.Currency(currency))
	if err != nil {
		return money.Money{}, xerr.Validation(xerr.CodeInvalidRequest, "Invalid amount",
			"$field must be a decimal amount with at most two fractional digits and an ISO 4217 currency.",
			map[string]any{"field": field, "amount": amount, "currency": currency})
	}
	return parsed, nil
}
