// Package httpx is the transport vocabulary of the API: the problem shape every
// failure is rendered in, and the middleware that carries correlation
// identifiers, limits payloads, recovers panics and records metrics.
package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/ironledger/iron-ledger/internal/platform/logging"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// Problem is the error body of every non-2xx response.
//
// The contract distinguishes the cases a caller must tell apart: a malformed
// request, a conflict with something already stored, a terminal business
// rejection carrying a stable code, work that was durably accepted but is not
// finished, and a dependency that is temporarily down. Each has its own status
// code and its own fields.
type Problem struct {
	Error         string         `json:"error"`
	Code          xerr.Code      `json:"code"`
	Title         string         `json:"title"`
	Message       string         `json:"message"`
	Params        map[string]any `json:"params,omitempty"`
	CorrelationID string         `json:"correlationId"`
	Retryable     bool           `json:"retryable"`
}

// WriteProblem renders err as a Problem, logs it, and writes it.
//
// The cause is logged but never the payload: a log line must never carry a full
// financial payload or any credential.
func WriteProblem(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}

	rejection, ok := xerr.As(err)
	if !ok {
		rejection = xerr.Infrastructure("The request could not be completed.", err)
	}

	status := rejection.Kind.HTTPStatus()
	problem := Problem{
		Error:         http.StatusText(status),
		Code:          rejection.Code,
		Title:         rejection.Title,
		Message:       rejection.Message,
		Params:        rejection.Params,
		CorrelationID: logging.CorrelationID(ctx),
		Retryable:     !rejection.Kind.Terminal(),
	}

	if logger != nil {
		level := slog.LevelWarn
		if status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		logger.Log(ctx, level, "request rejected",
			append([]any{
				"status", status,
				"code", string(rejection.Code),
				"kind", kindName(rejection.Kind),
				"cause", causeOf(rejection),
			}, loggingFields(ctx)...)...)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem)
}

// WriteJSON writes a success body.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func loggingFields(ctx context.Context) []any {
	fields := make([]any, 0, 10)
	for _, pair := range [][2]string{
		{logging.FieldCorrelationID, logging.CorrelationID(ctx)},
		{logging.FieldTransactionID, logging.TransactionID(ctx)},
		{logging.FieldWalletID, logging.WalletID(ctx)},
		{logging.FieldProviderID, logging.ProviderID(ctx)},
	} {
		if pair[1] != "" {
			fields = append(fields, pair[0], pair[1])
		}
	}
	return fields
}

func causeOf(err *xerr.Error) string {
	if cause := err.Unwrap(); cause != nil {
		return cause.Error()
	}
	return ""
}

func kindName(kind xerr.Kind) string {
	switch kind {
	case xerr.KindValidation:
		return "validation"
	case xerr.KindBusinessRule:
		return "business_rule"
	case xerr.KindNotFound:
		return "not_found"
	case xerr.KindConflict:
		return "conflict"
	case xerr.KindUnauthenticated:
		return "unauthenticated"
	case xerr.KindForbidden:
		return "forbidden"
	case xerr.KindPending:
		return "pending"
	case xerr.KindUnavailable:
		return "unavailable"
	case xerr.KindConcurrency:
		return "concurrency"
	default:
		return "internal"
	}
}

// WithCorrelationID assigns a correlation identifier to every request and
// echoes it in the response headers, so a caller can quote it in a support
// ticket and an operator can find the operation in the logs.
func WithCorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlationID := r.Header.Get("X-Correlation-Id")
		if correlationID == "" {
			correlationID = newCorrelationID()
		}
		w.Header().Set("X-Correlation-Id", correlationID)
		next.ServeHTTP(w, r.WithContext(logging.WithCorrelationID(r.Context(), correlationID)))
	})
}

func newCorrelationID() string {
	if token, err := newToken(); err == nil {
		return token
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// Recorder captures the status code so the observability middleware can
// classify the outcome.
type Recorder struct {
	http.ResponseWriter
	Status int
}

// WriteHeader records the status code.
func (r *Recorder) WriteHeader(status int) {
	r.Status = status
	r.ResponseWriter.WriteHeader(status)
}

// Write keeps the recorder usable as a body writer.
func (r *Recorder) Write(b []byte) (int, error) {
	if r.Status == 0 {
		r.Status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// WithObservability records the status and duration of every request.
func WithObservability(logger *slog.Logger, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			recorder := &Recorder{ResponseWriter: w}
			next.ServeHTTP(recorder, r)

			status := recorder.Status
			if status == 0 {
				status = http.StatusOK
			}
			if logger != nil {
				logger.Log(r.Context(), slog.LevelInfo, "request completed",
					logging.FieldCorrelationID, logging.CorrelationID(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"status", status,
					"durationMs", time.Since(started).Milliseconds(),
				)
			}
			if m != nil {
				m.OperationLatency.
					WithLabelValues("http", "http").
					Observe(time.Since(started).Seconds())
			}
		})
	}
}

// Recoverer turns a panic into a 500 rather than a dropped connection.
//
// A business rejection is always a returned error, never a panic: this exists
// only so a genuine bug in a handler cannot take the process down or leak a
// stack trace to a caller.
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					if logger != nil {
						logger.Log(r.Context(), slog.LevelError, "handler panicked",
							logging.FieldCorrelationID, logging.CorrelationID(r.Context()),
							"panic", recovered,
							"stack", string(debug.Stack()),
						)
					}
					WriteProblem(w, r, logger, xerr.Infrastructure(
						"The request could not be completed due to an internal error.", nil))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// LimitBody refuses a request whose body is larger than allowed, so an
// oversized payload cannot exhaust memory before validation runs.
func LimitBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				WriteProblem(w, r, nil, xerr.Validation(xerr.CodeInvalidRequest, "Payload too large",
					"The request body is larger than the $limit bytes this endpoint accepts.",
					map[string]any{"limit": strconv.FormatInt(limit, 10)}))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// Chain applies middlewares so the first argument is the outermost.
func Chain(handler http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}
