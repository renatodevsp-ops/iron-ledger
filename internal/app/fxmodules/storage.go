package fxmodules

import (
	"context"
	"time"

	"go.uber.org/fx"

	cqrs "github.com/terraskye/eventsourcing"

	wageringevents "github.com/ironledger/iron-ledger/internal/domain/wagering/events"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/awaitwagerreference"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/registerwageroperation"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/settlewageroperation"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/wagertransactiondetails"
	walletevents "github.com/ironledger/iron-ledger/internal/domain/wallet/events"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/applywalletmovement"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/openwallet"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/walletdetails"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/walletledgerentries"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/walletreconciliation"
	"github.com/ironledger/iron-ledger/internal/messaging/inbox"
	"github.com/ironledger/iron-ledger/internal/messaging/outbox"
	"github.com/ironledger/iron-ledger/internal/messaging/sqs"
	"github.com/ironledger/iron-ledger/internal/platform/config"
	"github.com/ironledger/iron-ledger/internal/platform/integration"
	"github.com/ironledger/iron-ledger/internal/platform/pgdb"
	"github.com/ironledger/iron-ledger/internal/platform/support"
)

// StorageModule provides the projections of every bounded context.
//
// Each read model is one slice, with its own projector and repository, so a
// bounded context can be read, replayed or replaced without touching the others.
var StorageModule = fx.Module("storage",
	// Wallet context.
	fx.Provide(walletdetails.NewRepo),
	fx.Provide(walletledgerentries.NewRepo),
	fx.Provide(AsLedgerRepository),
	fx.Provide(walletreconciliation.NewWalletAdapter),
	fx.Provide(AsWalletFinder),
	fx.Provide(AsWalletBalances),
	fx.Provide(walletreconciliation.New),

	// Wagering context.
	fx.Provide(wagertransactiondetails.NewRepo),

	// Messaging context.
	fx.Provide(inbox.NewRepo),
	fx.Provide(AsInboxRepository),
	fx.Provide(outbox.NewRepo),
	fx.Provide(AsOutboxRepository),
)

// AsWalletFinder binds the wallet projection to the reader the adapter needs.
func AsWalletFinder(repo *walletdetails.Repo) walletreconciliation.WalletFinder { return repo }

// AsWalletBalances binds the wallet projection to the minimal view a
// reconciliation needs.
func AsWalletBalances(adapter *walletreconciliation.WalletAdapter) walletreconciliation.WalletBalances {
	return adapter
}

// AsLedgerRepository binds the ledger to its read interface.
func AsLedgerRepository(repo *walletledgerentries.Repo) walletledgerentries.Repository { return repo }

// AsInboxRepository binds the inbox store to its interface.
func AsInboxRepository(repo *inbox.Repo) inbox.Repository { return repo }

// AsOutboxRepository binds the outbox store to its interface.
func AsOutboxRepository(repo *outbox.Repo) outbox.Repository { return repo }

// SlicesModule provides the command handlers of every bounded context.
//
// Every handler receives the same metadata extractors, so correlation and
// causation ids reach the event store — and therefore the published envelope —
// no matter which slice appended the event.
var SlicesModule = fx.Module("slices",
	fx.Provide(NewMetadataExtractors),

	// Wallet context.
	fx.Provide(NewOpenWalletHandler),
	fx.Provide(NewApplyWalletMovementHandler),

	// Wagering context.
	fx.Provide(NewRegisterWagerOperationHandler),
	fx.Provide(NewAwaitWagerReferenceHandler),
	fx.Provide(NewSettleWagerOperationHandler),
)

// NewMetadataExtractors builds the operational metadata every event carries.
func NewMetadataExtractors() support.MetadataExtractors {
	return support.MetadataExtractors{support.OperationalMetadata()}
}

// NewOpenWalletHandler builds the wallet-opening handler.
func NewOpenWalletHandler(store cqrs.EventStore, extractors support.MetadataExtractors) cqrs.CommandHandler[openwallet.Command] {
	return openwallet.NewCommandHandler(store, extractors...)
}

// NewApplyWalletMovementHandler builds the balance-movement handler.
func NewApplyWalletMovementHandler(store cqrs.EventStore, extractors support.MetadataExtractors) cqrs.CommandHandler[applywalletmovement.Command] {
	return applywalletmovement.NewCommandHandler(store, extractors...)
}

// NewRegisterWagerOperationHandler builds the operation-acceptance handler.
func NewRegisterWagerOperationHandler(store cqrs.EventStore, extractors support.MetadataExtractors) cqrs.CommandHandler[registerwageroperation.Command] {
	return registerwageroperation.NewCommandHandler(store, extractors...)
}

// NewAwaitWagerReferenceHandler builds the pending-reference handler.
func NewAwaitWagerReferenceHandler(store cqrs.EventStore, extractors support.MetadataExtractors) cqrs.CommandHandler[awaitwagerreference.Command] {
	return awaitwagerreference.NewCommandHandler(store, extractors...)
}

// NewSettleWagerOperationHandler builds the settlement handler.
func NewSettleWagerOperationHandler(store cqrs.EventStore, extractors support.MetadataExtractors) cqrs.CommandHandler[settlewageroperation.Command] {
	return settlewageroperation.NewCommandHandler(store, extractors...)
}

// MessagingModule provides the integration builders, the outbox dispatcher and
// the broker client.
var MessagingModule = fx.Module("messaging",
	fx.Provide(NewIntegrationBuilders),
	fx.Provide(NewOutboxDispatcher),
	fx.Provide(NewBroker),
)

// NewIntegrationBuilders composes the builders of every bounded context that
// publishes events.
//
// Adding a context to the publication contract means adding one builder here.
// Nothing else has to know the event exists.
func NewIntegrationBuilders() integration.MultiBuilder {
	return integration.MultiBuilder{
		walletevents.NewBuilder(),
		wageringevents.NewBuilder(),
	}
}

// NewOutboxDispatcher builds the transactional outbox dispatcher.
func NewOutboxDispatcher(repo outbox.Repository, builders integration.MultiBuilder) *outbox.Dispatcher {
	return outbox.NewDispatcher(repo, builders)
}

// NewBroker provisions the SQS client and the queues it needs.
//
// Provisioning happens here, in the composition root, so no worker can start
// against a queue that does not exist yet and every process agrees on the URLs.
func NewBroker(cfg config.Config) (*sqs.Runtime, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return sqs.Provision(ctx, cfg.SQS)
}

// Projectors returns the projector groups in the order they must run.
//
// The wallet projections come first: a wager transaction row references its
// wallet, so the wallet row has to exist before the transaction that names it is
// written. The order is part of the contract and is asserted by the integration
// tests.
func Projectors(resolver pgdb.Resolver) []*cqrs.EventGroupProcessor {
	return []*cqrs.EventGroupProcessor{
		walletdetails.NewGroup(resolver),
		walletledgerentries.NewGroup(resolver),
		wagertransactiondetails.NewGroup(resolver),
	}
}
