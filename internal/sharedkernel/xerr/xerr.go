// Package xerr defines the error taxonomy shared by every bounded context.
//
// Business rejections are values, not strings: each carries a stable failure
// code that is safe to expose to a game provider, a title, an actionable
// message and a set of parameters. Infrastructure failures keep their own
// sentinel errors so a caller can tell "the wallet cannot cover this bet"
// (terminal, 422) from "PostgreSQL is down" (transient, 503) from "two
// writers collided" (retryable, internal).
package xerr

import (
	"errors"
	"fmt"
	"net/http"
)

// Kind classifies an error for transport mapping and retry decisions.
type Kind int

const (
	// KindInternal is an unexpected failure. Treated as transient by callers
	// that can retry (the SQS consumer) and as 500 over HTTP.
	KindInternal Kind = iota
	// KindValidation is a malformed or semantically invalid request. 400.
	KindValidation
	// KindBusinessRule is a terminal business rejection. 422.
	KindBusinessRule
	// KindNotFound is a missing aggregate. 404.
	KindNotFound
	// KindConflict is a uniqueness/idempotency conflict. 409.
	KindConflict
	// KindUnauthenticated is a missing or invalid credential. 401.
	KindUnauthenticated
	// KindForbidden is an authenticated caller lacking permission. 403.
	KindForbidden
	// KindPending means the request was durably accepted but is not finished
	// yet. 202.
	KindPending
	// KindUnavailable is a transient dependency outage. 503.
	KindUnavailable
	// KindConcurrency is a lost-update race that must be retried. 409.
	KindConcurrency
)

// Code is a stable, documented failure code.
type Code string

// Business failure codes. These are part of the external contract: they are
// documented in ARCHITECTURE.md and never change meaning.
const (
	CodeInsufficientBalance         Code = "INSUFFICIENT_BALANCE"
	CodeReversalInsufficientFunds   Code = "REVERSAL_INSUFFICIENT_FUNDS"
	CodeAmountMustBePositive        Code = "AMOUNT_MUST_BE_POSITIVE"
	CodeLossAmountMustBeZero        Code = "LOSS_AMOUNT_MUST_BE_ZERO"
	CodeNegativeAmountNotAllowed    Code = "NEGATIVE_AMOUNT_NOT_ALLOWED"
	CodeCurrencyMismatch            Code = "CURRENCY_MISMATCH"
	CodeWalletNotFound              Code = "WALLET_NOT_FOUND"
	CodeWalletAlreadyExists         Code = "WALLET_ALREADY_EXISTS"
	CodeKindNotAccepted             Code = "KIND_NOT_ACCEPTED"
	CodeReferenceRequired           Code = "REFERENCE_REQUIRED"
	CodeReferenceNotFound           Code = "REFERENCE_NOT_FOUND"
	CodeReferenceNotProcessed       Code = "REFERENCE_NOT_PROCESSED"
	CodeReferenceKindMismatch       Code = "REFERENCE_KIND_MISMATCH"
	CodeReferenceAlreadyReversed    Code = "REFERENCE_ALREADY_REVERSED"
	CodeReferenceAmountMismatch     Code = "REFERENCE_AMOUNT_MISMATCH"
	CodeIdempotencyKeyConflict      Code = "IDEMPOTENCY_KEY_CONFLICT"
	CodeExternalTransactionConflict Code = "EXTERNAL_TRANSACTION_CONFLICT"
	CodeInfrastructureFailure       Code = "INFRASTRUCTURE_FAILURE"
	CodeInvalidRequest              Code = "INVALID_REQUEST"
	CodeProviderMismatch            Code = "PROVIDER_MISMATCH"
	CodeUnauthenticated             Code = "UNAUTHENTICATED"
	CodeForbidden                   Code = "FORBIDDEN"
	CodeResourceNotFound            Code = "RESOURCE_NOT_FOUND"
	CodeDependencyUnavailable       Code = "DEPENDENCY_UNAVAILABLE"
)

// Error is a business rejection carrying a stable code.
type Error struct {
	Kind    Kind
	Code    Code
	Title   string
	Message string
	Params  map[string]any
	cause   error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the underlying cause, when any.
func (e *Error) Unwrap() error { return e.cause }

// WithCause attaches a non-nil cause without changing the classification.
func (e *Error) WithCause(err error) *Error {
	if err == nil {
		return e
	}
	clone := *e
	clone.cause = err
	return &clone
}

// New builds a business rejection.
func New(kind Kind, code Code, title, message string, params map[string]any) *Error {
	return &Error{Kind: kind, Code: code, Title: title, Message: message, Params: params}
}

// Validation builds a 400-class rejection.
func Validation(code Code, title, message string, params map[string]any) *Error {
	return New(KindValidation, code, title, message, params)
}

// Rejected builds a terminal business rejection (422).
func Rejected(code Code, title, message string, params map[string]any) *Error {
	return New(KindBusinessRule, code, title, message, params)
}

// NotFound builds a 404-class rejection.
func NotFound(code Code, title, message string, params map[string]any) *Error {
	return New(KindNotFound, code, title, message, params)
}

// Conflict builds a 409-class rejection.
func Conflict(code Code, title, message string, params map[string]any) *Error {
	return New(KindConflict, code, title, message, params)
}

// Forbidden builds a 403-class rejection.
func Forbidden(code Code, title, message string, params map[string]any) *Error {
	return New(KindForbidden, code, title, message, params)
}

// Unauthenticated builds a 401-class rejection.
func Unauthenticated(code Code, title, message string, params map[string]any) *Error {
	return New(KindUnauthenticated, code, title, message, params)
}

// Pending builds a 202-class rejection for durably accepted, unfinished work.
func Pending(code Code, title, message string, params map[string]any) *Error {
	return New(KindPending, code, title, message, params)
}

// Unavailable builds a 503-class rejection.
func Unavailable(code Code, title, message string, params map[string]any) *Error {
	return New(KindUnavailable, code, title, message, params)
}

// Infrastructure builds a retryable failure that is *not* a business decision.
// It maps to 500 over HTTP and to an SQS retry.
func Infrastructure(message string, cause error) *Error {
	return (&Error{
		Kind:    KindInternal,
		Code:    CodeInfrastructureFailure,
		Title:   "Internal failure",
		Message: message,
	}).WithCause(cause)
}

// KindOf extracts the Kind of err, defaulting to KindInternal.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// CodeOf extracts the stable Code of err, if it carries one.
func CodeOf(err error) (Code, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return "", false
}

// As extracts the *Error of err, if it carries one.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// HTTPStatus maps a Kind onto the HTTP status code used by the API.
func (s Kind) HTTPStatus() int {
	switch s {
	case KindValidation:
		return http.StatusBadRequest
	case KindBusinessRule:
		return http.StatusUnprocessableEntity
	case KindNotFound:
		return http.StatusNotFound
	case KindConflict, KindConcurrency:
		return http.StatusConflict
	case KindUnauthenticated:
		return http.StatusUnauthorized
	case KindForbidden:
		return http.StatusForbidden
	case KindPending:
		return http.StatusAccepted
	case KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Terminal reports whether a rejection ends the life of a wager transaction.
// A terminal rejection lets the SQS consumer delete the message instead of
// retrying it.
func (k Kind) Terminal() bool {
	switch k {
	case KindValidation, KindBusinessRule, KindNotFound, KindConflict,
		KindUnauthenticated, KindForbidden:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether err is a rejection that must not be retried.
func IsTerminal(err error) bool {
	return KindOf(err).Terminal()
}
