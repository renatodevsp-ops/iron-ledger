package identity

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ironledger/iron-ledger/internal/platform/httpx"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

type ctxKey struct{}

// WithPrincipal returns ctx carrying the authenticated caller.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, principal)
}

// PrincipalFrom returns the authenticated caller of ctx, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(ctxKey{}).(Principal)
	return principal, ok
}

// Authenticate is the middleware that turns a bearer token into a principal.
//
// It never lets an unauthenticated request reach a business handler: a missing,
// malformed, expired or wrongly issued token is refused here with a stable
// error, before any handler can act on it.
func Authenticate(verifier *Verifier, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, err := bearerToken(r)
			if err != nil {
				unauthenticated(w, r, logger, err)
				return
			}
			principal, err := verifier.Verify(r.Context(), token)
			if err != nil {
				unauthenticated(w, r, logger, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

// bearerToken extracts the access token from the Authorization header.
func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", ErrNoCredentials
	}
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", ErrNoCredentials
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ErrNoCredentials
	}
	return value, nil
}

func unauthenticated(w http.ResponseWriter, r *http.Request, logger *slog.Logger, cause error) {
	rejection := xerr.Unauthenticated(xerr.CodeUnauthenticated, "Authentication required",
		"A valid OAuth 2.0 bearer token issued by the platform's identity provider is required.",
		nil).WithCause(cause)
	w.Header().Set("WWW-Authenticate", `Bearer realm="iron-ledger", error="invalid_token"`)
	httpx.WriteProblem(w, r, logger, rejection)
}

// RequireInternal refuses anyone who is not the internal service.
func RequireInternal(ctx context.Context) (Principal, error) {
	principal, ok := PrincipalFrom(ctx)
	if !ok {
		return Principal{}, xerr.Unauthenticated(xerr.CodeUnauthenticated, "Authentication required",
			"This operation requires a valid bearer token.", nil)
	}
	if !principal.Internal {
		return Principal{}, xerr.Forbidden(xerr.CodeForbidden, "Internal operation only",
			"This operation is restricted to the platform's internal service. The caller acts as provider $provider.",
			map[string]any{"provider": principal.ProviderID})
	}
	return principal, nil
}

// RequireProvider returns the provider the caller may act as.
//
// The provider comes from the token, never from the request body: a provider
// cannot submit operations under another provider's identity by sending a
// different providerId.
func RequireProvider(ctx context.Context) (Principal, error) {
	principal, ok := PrincipalFrom(ctx)
	if !ok {
		return Principal{}, xerr.Unauthenticated(xerr.CodeUnauthenticated, "Authentication required",
			"This operation requires a valid bearer token.", nil)
	}
	if principal.Internal {
		return Principal{}, xerr.Forbidden(xerr.CodeForbidden, "Provider operation only",
			"Wallet operations are restricted to the platform's internal service.", nil)
	}
	if principal.ProviderID == "" {
		return Principal{}, xerr.Forbidden(xerr.CodeProviderMismatch, "Provider not authorised",
			"The presented credential is not mapped to a game provider.", nil)
	}
	return principal, nil
}

// RequireProviderOwns refuses a caller that does not own the requested
// provider.
//
// It returns 403 rather than 404: the caller named a provider explicitly, and
// the answer it gets must not depend on whether that provider's transactions
// exist.
func RequireProviderOwns(ctx context.Context, providerID string) (Principal, error) {
	principal, err := RequireProvider(ctx)
	if err != nil {
		return Principal{}, err
	}
	if principal.ProviderID != providerID {
		return Principal{}, xerr.Forbidden(xerr.CodeProviderMismatch, "Provider not authorised",
			"This credential may only act as provider $own, not as $requested.",
			map[string]any{"own": principal.ProviderID, "requested": providerID})
	}
	return principal, nil
}

// RequireProviderOrInternal allows both the internal service and the provider
// that owns the resource.
func RequireProviderOrInternal(ctx context.Context, providerID string) (Principal, error) {
	principal, ok := PrincipalFrom(ctx)
	if !ok {
		return Principal{}, xerr.Unauthenticated(xerr.CodeUnauthenticated, "Authentication required",
			"This operation requires a valid bearer token.", nil)
	}
	if principal.Internal {
		return principal, nil
	}
	if principal.ProviderID != providerID {
		// 404, not 403: revealing that another provider's transaction exists
		// would leak data across the isolation boundary.
		return Principal{}, xerr.NotFound(xerr.CodeResourceNotFound, "Transaction not found",
			"Transaction $transaction does not exist.",
			map[string]any{"transaction": providerID})
	}
	return principal, nil
}
