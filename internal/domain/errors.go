package domain

import (
	"errors"
	"fmt"
)

// ReasonCode is the stable, machine-readable rejection reason published in
// contracts/openapi.yaml. Callers branch on this and never on the message.
type ReasonCode string

const (
	// Business and validation reasons (also listed in tasks.md "Governing constraints").
	ReasonInsufficientFunds      ReasonCode = "INSUFFICIENT_FUNDS"
	ReasonBetNotFound            ReasonCode = "BET_NOT_FOUND"
	ReasonBetAlreadySettled      ReasonCode = "BET_ALREADY_SETTLED"
	ReasonCurrencyMismatch       ReasonCode = "CURRENCY_MISMATCH"
	ReasonWalletFrozen           ReasonCode = "WALLET_FROZEN"
	ReasonInvalidAmount          ReasonCode = "INVALID_AMOUNT"
	ReasonInvalidCurrency        ReasonCode = "INVALID_CURRENCY"
	ReasonIdempotencyKeyConflict ReasonCode = "IDEMPOTENCY_KEY_CONFLICT"
	ReasonDuplicateTransactionID ReasonCode = "DUPLICATE_TRANSACTION_ID"
	ReasonReferenceAlreadyExists ReasonCode = "REFERENCE_ALREADY_EXISTS"
	ReasonInvalidOperationState  ReasonCode = "INVALID_OPERATION_STATE"

	// Transport reasons defined in contracts/openapi.yaml.
	ReasonMissingIdempotencyKey ReasonCode = "MISSING_IDEMPOTENCY_KEY"
	ReasonUnknownOperationType  ReasonCode = "UNKNOWN_OPERATION_TYPE"
	ReasonUnknownField          ReasonCode = "UNKNOWN_FIELD"
	ReasonMissingField          ReasonCode = "MISSING_FIELD"
	ReasonWalletNotFound        ReasonCode = "WALLET_NOT_FOUND"
	ReasonUnauthorized          ReasonCode = "UNAUTHORIZED"
	ReasonForbidden             ReasonCode = "FORBIDDEN"
	ReasonTenantMismatch        ReasonCode = "TENANT_MISMATCH"
	ReasonRateLimited           ReasonCode = "RATE_LIMITED"
	ReasonDependencyUnavailable ReasonCode = "DEPENDENCY_UNAVAILABLE"
	ReasonInternalError         ReasonCode = "INTERNAL_ERROR"
)

// Error is the single typed error of the domain. Every rejection carries a
// stable ReasonCode; the optional detail fields mirror the contract so a
// caller can size a failure without issuing a second request.
type Error struct {
	Code           ReasonCode
	Message        string
	Field          string
	AvailableMinor *int64
	WalletCurrency *Currency
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// Is matches two domain errors by their ReasonCode, so callers can use
// errors.Is against the sentinels below regardless of the detail payload.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Code == e.Code
}

// NewError builds a domain error. It is exported so platform adapters can
// translate driver-level failures (unique/check violations, missing rows)
// into the same typed error without importing anything from the domain.
func NewError(code ReasonCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithField returns a copy carrying the offending request field.
func (e *Error) WithField(field string) *Error {
	c := *e
	c.Field = field
	return &c
}

// WithAvailableMinor returns a copy carrying the current available balance.
func (e *Error) WithAvailableMinor(minor int64) *Error {
	c := *e
	c.AvailableMinor = &minor
	return &c
}

// WithWalletCurrency returns a copy carrying the wallet's own currency.
func (e *Error) WithWalletCurrency(c Currency) *Error {
	cp := c
	cc := *e
	cc.WalletCurrency = &cp
	return &cc
}

// Sentinels for errors.Is. The message is only a default; callers that know
// more context should build a specific error with the same code.
var (
	ErrInsufficientFunds      = NewError(ReasonInsufficientFunds, "insufficient funds")
	ErrBetNotFound            = NewError(ReasonBetNotFound, "bet not found")
	ErrBetAlreadySettled      = NewError(ReasonBetAlreadySettled, "bet is already settled")
	ErrCurrencyMismatch       = NewError(ReasonCurrencyMismatch, "currency does not match the wallet")
	ErrWalletFrozen           = NewError(ReasonWalletFrozen, "wallet is FROZEN")
	ErrInvalidAmount          = NewError(ReasonInvalidAmount, "amount must be a positive integer in the currency minor unit")
	ErrInvalidCurrency        = NewError(ReasonInvalidCurrency, "unsupported currency")
	ErrIdempotencyKeyConflict = NewError(ReasonIdempotencyKeyConflict, "idempotency key was used with a different request body")
	ErrDuplicateTransactionID = NewError(ReasonDuplicateTransactionID, "transaction id already used for this wallet")
	ErrReferenceAlreadyExists = NewError(ReasonReferenceAlreadyExists, "reference id already exists for this tenant")
	ErrInvalidOperationState  = NewError(ReasonInvalidOperationState, "operation is not valid in the current state")
	ErrWalletNotFound         = NewError(ReasonWalletNotFound, "wallet not found")
	ErrMissingIdempotencyKey  = NewError(ReasonMissingIdempotencyKey, "Idempotency-Key header is required")
	ErrUnauthorized           = NewError(ReasonUnauthorized, "missing or unverifiable access token")
	ErrForbidden              = NewError(ReasonForbidden, "token is not permitted for this operation")
	ErrTenantMismatch         = NewError(ReasonTenantMismatch, "token tenant does not own this resource")
	ErrDependencyUnavailable  = NewError(ReasonDependencyUnavailable, "a required dependency is unavailable")
	ErrInternalError          = NewError(ReasonInternalError, "internal error")
)

// CodeOf extracts the ReasonCode from an error, or ReasonInternalError when the
// error is not a typed domain error.
func CodeOf(err error) ReasonCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return ReasonInternalError
}
