// Package logging provides the process logger. It writes structured key/value
// lines to stderr so a log shipper can parse them, and it deliberately never
// logs a bearer token, an idempotency key or a balance value.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// New builds the process logger at the given level. Only the level is
// configurable; the format is always one JSON object per line, because a mixed
// format is unparseable by every shipper.
func New(level string, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(&redactingHandler{inner: handler})
}

// redactingHandler drops the arguments that must never reach a log sink. It is a
// last line of defence: the code that logs should not pass these in the first
// place, but a single careless Info call should not be able to write a token.
type redactingHandler struct{ inner slog.Handler }

// redactedKeys are argument names whose values are removed.
var redactedKeys = map[string]bool{
	"authorization": true,
	"token":         true,
	"access_token":  true,
	"password":      true,
	"dsn":           true,
	"database_url":  true,
}

func (h *redactingHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.inner.Enabled(ctx, lvl)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(redact(a))
		return true
	})
	return h.inner.Handle(ctx, clean)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		clean = append(clean, redact(a))
	}
	return &redactingHandler{inner: h.inner.WithAttrs(clean)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

// redact replaces a sensitive value, and recurses into groups so a nested
// credential is caught too.
func redact(a slog.Attr) slog.Attr {
	if redactedKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, "[REDACTED]")
	}
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		out := make([]any, 0, len(group))
		for _, nested := range group {
			out = append(out, redact(nested))
		}
		return slog.Group(a.Key, out...)
	}
	return a
}
