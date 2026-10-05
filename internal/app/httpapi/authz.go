package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/app/usecase"
	"github.com/ironledger/iron-ledger/internal/identity"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// providerFor resolves the provider a request acts as.
//
// The credential decides. A providerId in the body is only cross-checked, and a
// mismatch is a 403 rather than a silent substitution: a provider must never be
// able to submit an operation under someone else's identity by editing a field.
func (h *Handler) providerFor(w http.ResponseWriter, r *http.Request, bodyProviderID string) (identity.Principal, error) {
	principal, err := identity.RequireProvider(r.Context())
	if err != nil {
		return identity.Principal{}, err
	}

	if bodyProviderID != "" && bodyProviderID != principal.ProviderID {
		return identity.Principal{}, xerr.Forbidden(xerr.CodeProviderMismatch, "Provider not authorised",
			"This credential acts as provider $own and cannot act as $requested.",
			map[string]any{"own": principal.ProviderID, "requested": bodyProviderID})
	}
	return principal, nil
}

// lookupTransaction reads a transaction, hiding it from a provider that does not
// own it.
//
// A provider asking for another provider's transaction gets 404, not 403:
// confirming that the transaction exists would already leak data across the
// isolation boundary.
func (h *Handler) lookupTransaction(r *http.Request, transactionID string) (*usecase.TransactionView, error) {
	principal, err := h.principal(r)
	if err != nil {
		return nil, err
	}
	id := uuid.MustParse(transactionID)

	if !principal.Internal {
		owner, err := h.readQueries.OwnerProviderOf(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if owner != principal.ProviderID {
			return nil, xerr.NotFound(xerr.CodeResourceNotFound, "Transaction not found",
				"Transaction $transaction does not exist.",
				map[string]any{"transaction": transactionID})
		}
	}
	return h.readQueries.GetTransaction(r.Context(), id)
}

// lookupProviderTransaction reads a transaction by the provider's own identity.
func (h *Handler) lookupProviderTransaction(r *http.Request, providerID, externalID string) (*usecase.TransactionView, error) {
	if _, err := identity.RequireProviderOwns(r.Context(), providerID); err != nil {
		return nil, err
	}
	return h.readQueries.GetProviderTransaction(r.Context(), providerID, externalID)
}

func (h *Handler) principal(r *http.Request) (identity.Principal, error) {
	principal, ok := identity.PrincipalFrom(r.Context())
	if !ok {
		return identity.Principal{}, xerr.Unauthenticated(xerr.CodeUnauthenticated, "Authentication required",
			"This operation requires a valid bearer token.", nil)
	}
	return principal, nil
}

func parseUUID(raw, field string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, xerr.Validation(xerr.CodeInvalidRequest, "Invalid identifier",
			"$field must be a UUID.", map[string]any{"field": field, "value": raw})
	}
	return parsed, nil
}

func pathUUID(r *http.Request, param string) (uuid.UUID, error) {
	return parseUUID(chi.URLParam(r, param), param)
}

func queryInt(r *http.Request, param string) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(param))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, xerr.Validation(xerr.CodeInvalidRequest, "Invalid pagination parameter",
			"$$param must be a non-negative integer.", map[string]any{param: raw})
	}
	return value, nil
}
