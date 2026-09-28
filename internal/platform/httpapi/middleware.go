package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/ironledger/ironledger/internal/platform/idgen"
)

type contextKey struct{}

// withRequestID assigns every request a correlation id and echoes it back. The
// id is generated here rather than trusted from a header, so a caller cannot
// forge a value that ties their request to someone else's log lines.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := idgen.New().NewUUID()
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), contextKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recoverPanic turns a panic into a 500 instead of a dropped connection. It is
// the outermost handler so it also catches a panic in any middleware.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if s.log != nil {
					s.log.Error("panic serving request",
						"request_id", requestIDFrom(r.Context()),
						"method", r.Method,
						"path", r.URL.Path,
						"panic", rec)
				}
				// The message is deliberately generic: a panic value can carry a
				// query string or a decoded token.
				writeError(w, domain.ErrInternalError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code for the access log without buffering
// the body, so logging costs nothing on the response path.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health probes are excluded: they arrive every few seconds and would
		// bury the requests an operator is actually looking for.
		if strings.HasPrefix(r.URL.Path, "/health/") {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		if s.log != nil {
			s.log.Info("request",
				"request_id", requestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(started).Milliseconds())
		}
	})
}

// authenticate verifies the bearer token and stores the principal. A token that
// does not verify is a 401 and the request stops here: nothing downstream can
// do anything useful without a tenant.
func (s *Server) authenticate(next http.Handler, requiredScope string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := s.principalFromRequest(r)
		if err != nil {
			writeError(w, err)
			return
		}
		if requiredScope != "" && !principal.HasScope(requiredScope) {
			writeError(w, domain.NewError(domain.ReasonForbidden,
				"token is missing the %s scope", requiredScope))
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// principalKey is a distinct type from the request-id key so the two values can
// never be confused for one another in a context.
type principalKey struct{}

// principalFrom returns the authenticated caller attached by authenticate.
func principalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// principalFromRequest extracts and verifies the bearer token.
func (s *Server) principalFromRequest(r *http.Request) (Principal, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return Principal{}, domain.ErrUnauthorized
	}
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return Principal{}, domain.NewError(domain.ReasonUnauthorized,
			"Authorization header must use the Bearer scheme")
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return Principal{}, domain.ErrUnauthorized
	}
	if s.verifier == nil {
		return Principal{}, domain.ErrUnauthorized
	}
	principal, err := s.verifier.Verify(r.Context(), token)
	if err != nil {
		// The verifier's own error is discarded: a JWT failure detail can reveal
		// the expected issuer or key, which helps an attacker and helps nobody
		// debugging. The request id in the log is the way to correlate.
		return Principal{}, domain.ErrUnauthorized
	}
	return principal, nil
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}
