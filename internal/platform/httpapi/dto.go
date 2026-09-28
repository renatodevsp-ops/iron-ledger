// Package httpapi holds the REST transport. It translates between the wire
// contract in contracts/openapi.yaml and the domain, and it is the only place
// that knows about status codes, headers and JSON shapes.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ironledger/ironledger/internal/domain"
)

// statusFor maps a stable reason code onto the status the contract publishes.
// The mapping is a table rather than a range check because two codes that mean
// similar things deliberately land on different statuses: a missing key is a
// 400 (fix the request), a conflicting one is a 409 (the request is well formed
// but the state refuses it).
func statusFor(code domain.ReasonCode) int {
	switch code {
	case domain.ReasonMissingIdempotencyKey,
		domain.ReasonInvalidAmount,
		domain.ReasonInvalidCurrency,
		domain.ReasonUnknownOperationType,
		domain.ReasonUnknownField,
		domain.ReasonMissingField,
		domain.ReasonInvalidOperationState:
		return http.StatusBadRequest

	case domain.ReasonUnauthorized:
		return http.StatusUnauthorized

	case domain.ReasonForbidden, domain.ReasonTenantMismatch:
		return http.StatusForbidden

	case domain.ReasonRateLimited:
		return http.StatusTooManyRequests

	case domain.ReasonDependencyUnavailable:
		// The request was not applied, and the same key is safe to retry.
		return http.StatusServiceUnavailable

	case domain.ReasonInsufficientFunds,
		domain.ReasonWalletFrozen,
		domain.ReasonCurrencyMismatch,
		domain.ReasonIdempotencyKeyConflict,
		domain.ReasonDuplicateTransactionID,
		domain.ReasonReferenceAlreadyExists,
		domain.ReasonBetAlreadySettled,
		domain.ReasonBetNotFound:
		return http.StatusConflict

	case domain.ReasonWalletNotFound:
		return http.StatusNotFound

	default:
		return http.StatusInternalServerError
	}
}

// errorBody is the Error schema. The detail fields are pointers and omitempty so
// a caller can rely on a field being present exactly when it is meaningful,
// rather than on a zero value that looks like real data.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code           domain.ReasonCode `json:"code"`
	Message        string            `json:"message"`
	Field          string            `json:"field,omitempty"`
	AvailableMinor *int64            `json:"availableMinor,omitempty"`
	WalletCurrency *domain.Currency  `json:"walletCurrency,omitempty"`
}

// writeJSON emits a response with an explicit content type. It is used instead
// of writing to w directly so no handler can accidentally return a body with no
// content type, which clients treat as opaque.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		// Marshalling our own types cannot fail, but a silent empty body would be
		// worse than an explicit failure: report it as a server error.
		http.Error(w, `{"error":{"code":"INTERNAL_ERROR","message":"encode response"}}`,
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError maps a domain error onto the contract's status and envelope.
func writeError(w http.ResponseWriter, err error) {
	var domErr *domain.Error
	if !errors.As(err, &domErr) {
		// A non-typed error is a bug in this service, not a caller mistake. It is
		// reported as a 500 with a generic message: the underlying text may carry
		// connection strings or SQL, which must never reach a client.
		domErr = domain.ErrInternalError
	}
	writeJSON(w, statusFor(domErr.Code), errorBody{Error: errorDetail{
		Code:           domErr.Code,
		Message:        domErr.Message,
		Field:          domErr.Field,
		AvailableMinor: domErr.AvailableMinor,
		WalletCurrency: domErr.WalletCurrency,
	}})
}

// isUUID reports whether s has the canonical 8-4-4-4-12 hexadecimal form. It is
// checked before touching the database so a malformed id is a 404 rather than a
// 22P02 cast failure that would surface as a 500.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}
