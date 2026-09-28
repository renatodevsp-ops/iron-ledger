package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/ironledger/ironledger/internal/usecase"
)

// Principal is the authenticated caller, derived from a verified token. It is
// the only thing the service trusts about who is calling: the tenant comes from
// the token, never from a header, a path or a body, so a caller cannot reach
// another tenant's wallet by asking nicely.
type Principal struct {
	TenantID string
	Subject  string
	// ClientID is the token's `azp`. It becomes the audit `actor`, because it is
	// the identity an operator investigating a refusal actually needs.
	ClientID string
	Scopes   map[string]bool
}

// HasScope reports whether the token carried the required scope.
func (p Principal) HasScope(scope string) bool { return p.Scopes[scope] }

// TokenVerifier validates a bearer token and returns its principal. It is an
// interface so this package does not depend on Keycloak: the test double is a
// struct literal, and a second verifier is a drop-in.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (Principal, error)
}

// HealthCheck reports whether a dependency the service needs is usable. The
// readiness probe uses it; a function keeps the server free of any client type.
type HealthCheck func(ctx context.Context) error

// Logger is the structured logger the transport writes through. An interface
// keeps this package free of any particular logging library.
type Logger interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}

// Config carries the transport's tunables.
type Config struct {
	// ListenAddr is the address the transport binds.
	ListenAddr        string
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	// WriteScope is the scope a token must carry to mutate a wallet.
	WriteScope string
	// ReadScope is the scope a token must carry to read a wallet.
	ReadScope string
}

// DefaultConfig returns the transport settings. ReadHeaderTimeout is short on
// purpose: it is the defence against a slow-header connection occupying a worker
// without ever sending a request.
func DefaultConfig() Config {
	return Config{
		ListenAddr:        ":8080",
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		WriteScope:        "wallet:write",
		ReadScope:         "wallet:read",
	}
}

// Server owns the HTTP routes. It holds the application service and the token
// verifier and nothing else: no database handle, no SQL, no business rules.
type Server struct {
	svc                *usecase.Service
	verifier           TokenVerifier
	health             HealthCheck
	log                Logger
	requiredWriteScope string
	requiredReadScope  string
}

// New builds the transport. The returned *http.Server has its timeouts already
// set, because the safe values must not depend on a caller remembering them.
func New(cfg Config, svc *usecase.Service, verifier TokenVerifier, health HealthCheck, log Logger) *http.Server {
	s := &Server{
		svc:                svc,
		verifier:           verifier,
		health:             health,
		log:                log,
		requiredWriteScope: cfg.WriteScope,
		requiredReadScope:  cfg.ReadScope,
	}
	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           s.Handler(),
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
}

// Handler builds the route table and wraps it in the middleware chain, in the
// order the plan requires: request id, recovery, logging, then auth. Auth comes
// last so that a rejected request is still logged with a request id.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The mutating route needs the write scope and the read route the read
	// scope. The health probes are deliberately unauthenticated: an orchestrator
	// probes them without credentials, and they expose nothing a caller could act
	// on beyond "is this pod serving".
	mux.Handle("POST /v1/wallets/{walletId}/operations",
		s.authenticate(http.HandlerFunc(s.applyOperation), s.requiredWriteScope))
	mux.Handle("GET /v1/wallets/{walletId}",
		s.authenticate(http.HandlerFunc(s.getWallet), s.requiredReadScope))
	mux.HandleFunc("GET /health/live", s.liveness)
	mux.HandleFunc("GET /health/ready", s.readiness)

	return s.recoverPanic(s.logRequests(s.withRequestID(mux)))
}
