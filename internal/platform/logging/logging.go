// Package logging provides the structured JSON logger and the request-scoped
// identifiers that make a single wagering operation traceable across an HTTP
// handler, a SQL transaction, an SQS message and a published event.
package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

// Field names that carry operation identity. They are hoisted to the top level
// of every record emitted while the context is active.
const (
	FieldCorrelationID = "correlationId"
	FieldMessageID     = "messageId"
	FieldTransactionID = "transactionId"
	FieldWalletID      = "walletId"
	FieldProviderID    = "providerId"
	FieldComponent     = "component"
)

type ctxKey struct{ name string }

// WithCorrelationID returns ctx carrying a correlation identifier. When no id
// is supplied a new one is minted, so every log line of one operation shares
// it.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	if id == "" {
		id = newCorrelationID()
	}
	return context.WithValue(ctx, ctxKey{FieldCorrelationID}, id)
}

// CorrelationID returns the correlation identifier of ctx, or "".
func CorrelationID(ctx context.Context) string { return stringValue(ctx, FieldCorrelationID) }

// WithMessageID returns ctx carrying the broker message identifier.
func WithMessageID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{FieldMessageID}, id)
}

// MessageID returns the broker message identifier of ctx, or "".
func MessageID(ctx context.Context) string { return stringValue(ctx, FieldMessageID) }

// WithTransactionID returns ctx carrying the internal wager transaction id.
func WithTransactionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{FieldTransactionID}, id)
}

// TransactionID returns the wager transaction id of ctx, or "".
func TransactionID(ctx context.Context) string { return stringValue(ctx, FieldTransactionID) }

// WithWalletID returns ctx carrying the wallet id.
func WithWalletID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{FieldWalletID}, id)
}

// WalletID returns the wallet id of ctx, or "".
func WalletID(ctx context.Context) string { return stringValue(ctx, FieldWalletID) }

// WithProviderID returns ctx carrying the provider id.
func WithProviderID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{FieldProviderID}, id)
}

// ProviderID returns the provider id of ctx, or "".
func ProviderID(ctx context.Context) string { return stringValue(ctx, FieldProviderID) }

func stringValue(ctx context.Context, name string) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxKey{name}).(string)
	return v
}

// correlationCounter makes identifiers unique inside a process. It is atomic
// because every request mints one, concurrently.
var correlationCounter atomic.Uint64

func newCorrelationID() string {
	sequence := correlationCounter.Add(1)
	host, _ := os.Hostname()
	if idx := strings.IndexByte(host, '.'); idx > 0 {
		host = host[:idx]
	}
	if host == "" {
		host = "iron-ledger"
	}
	return host + "-" + itoa(sequence)
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// New builds the root logger. Every record is JSON on stdout.
func New(service, environment, level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(level)})).
		With(FieldComponent, service).
		With("environment", environment)
}

// With returns a logger tagged with an extra field.
func With(logger *slog.Logger, key, value string) *slog.Logger {
	return logger.With(key, value)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
