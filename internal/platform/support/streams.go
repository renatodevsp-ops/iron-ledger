// Package support holds the cross-slice plumbing of the ledger: stream
// naming and the tenant carried by a request context.
package support

import (
	"context"
	"fmt"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/platform/integration"
	"github.com/ironledger/iron-ledger/internal/platform/logging"
)

// StreamNamespace is the leading segment of every event stream id. Streams of
// different namespaces can never collide even when an aggregate id happens to
// repeat across bounded contexts.
const StreamNamespace = "ironledger"

// Bounded contexts owning event streams.
const (
	BoundedContextWallet   = "wallet"
	BoundedContextWagering = "wagering"
)

type tenantKey struct{}

// WithTenant returns ctx carrying the tenant every stream of this operation
// belongs to.
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenant)
}

// Tenant returns the tenant of ctx, defaulting to "default".
func Tenant(ctx context.Context) string {
	if v, ok := ctx.Value(tenantKey{}).(string); ok && v != "" {
		return v
	}
	return "default"
}

// StreamNamer builds the stream name of an aggregate.
//
// The format is
//
//	{namespace}-{tenant}-{bounded-context}-v1-{aggregate-plural}-{aggregate-id}
//
// It is the contract every slice that touches the same aggregate shares, which
// is what lets two different commands land on the same stream.
func StreamNamer(boundedContext, aggregatePlural string) cqrs.StreamNamer {
	return func(ctx context.Context, cmd cqrs.Command) string {
		return fmt.Sprintf("%s-%s-%s-v1-%s-%s",
			StreamNamespace, Tenant(ctx), boundedContext, aggregatePlural, cmd.AggregateID())
	}
}

// MetadataExtractors is the set of functions that stamp every appended event
// with the identifiers of the operation that produced it.
//
// The correlation and causation chain is what makes a published event
// traceable: correlationId follows one business operation across an HTTP
// request, a SQL transaction, an inbound message and every event it caused;
// causationId names the specific event or command that caused this one.
type MetadataExtractors []func(context.Context) map[string]any

// OperationalMetadata is the extractor the application installs on every
// command handler.
func OperationalMetadata() func(context.Context) map[string]any {
	return func(ctx context.Context) map[string]any {
		return map[string]any{
			integration.MetaCorrelationID: logging.CorrelationID(ctx),
			integration.MetaCausationID:   cqrs.CausationFromContext(ctx),
			integration.MetaTransactionID: logging.TransactionID(ctx),
			integration.MetaWalletID:      logging.WalletID(ctx),
			integration.MetaProviderID:    logging.ProviderID(ctx),
		}
	}
}

// WalletStream names the stream of the Wallet aggregate.
var WalletStream = StreamNamer(BoundedContextWallet, "wallets")

// WagerTransactionStream names the stream of the WagerTransaction aggregate.
var WagerTransactionStream = StreamNamer(BoundedContextWagering, "wager-transactions")
