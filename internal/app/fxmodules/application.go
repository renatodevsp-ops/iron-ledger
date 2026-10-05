package fxmodules

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"go.uber.org/fx"

	"github.com/jackc/pgx/v5/pgxpool"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/app/health"
	"github.com/ironledger/iron-ledger/internal/app/httpapi"
	"github.com/ironledger/iron-ledger/internal/app/pipeline"
	"github.com/ironledger/iron-ledger/internal/app/usecase"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/awaitwagerreference"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/registerwageroperation"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/settlewageroperation"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/slices/wagertransactiondetails"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/applywalletmovement"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/openwallet"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/walletdetails"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/walletledgerentries"
	"github.com/ironledger/iron-ledger/internal/domain/wallet/slices/walletreconciliation"
	"github.com/ironledger/iron-ledger/internal/identity"
	"github.com/ironledger/iron-ledger/internal/messaging/inbox"
	"github.com/ironledger/iron-ledger/internal/messaging/outbox"
	"github.com/ironledger/iron-ledger/internal/messaging/sqs"
	"github.com/ironledger/iron-ledger/internal/platform/config"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
	"github.com/ironledger/iron-ledger/internal/platform/pgdb"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
)

// ApplicationModule provides the use cases and the HTTP handler.
var ApplicationModule = fx.Module("application",
	fx.Provide(NewMetricsHandler),
	fx.Provide(AsWalletReader),
	fx.Provide(AsLedgerSums),
	fx.Provide(AsDivergenceObserver),
	fx.Provide(AsTransactionReader),
	fx.Provide(NewPipeline),
	fx.Provide(NewWageringUseCase),
	fx.Provide(NewWalletsUseCase),
	fx.Provide(NewLedgerUseCase),
	fx.Provide(usecase.NewQueries),
	fx.Provide(NewHandler),
	fx.Provide(NewHealthService),
)

// AsWalletReader binds the wallet projection to the narrow interface the use
// cases depend on.
func AsWalletReader(repo *walletdetails.Repo) usecase.WalletReader { return repo }

// AsLedgerSums binds the ledger to the aggregate the reconciliation needs.
func AsLedgerSums(repo *walletledgerentries.Repo) walletreconciliation.LedgerSums { return repo }

// AsDivergenceObserver reports reconciliation divergences to the metrics
// registry, so a silent divergence still moves an alarm.
func AsDivergenceObserver(m *metrics.Metrics) walletreconciliation.DivergenceObserver {
	return usecase.ReconciliationObserver{Metrics: m}
}

// AsTransactionReader binds the transaction index to the narrow interface the
// use cases depend on.
func AsTransactionReader(repo *wagertransactiondetails.Repo) usecase.TransactionReader {
	return repo
}

// NewPipeline builds the projection and publication pipeline.
func NewPipeline(logger *slog.Logger, dispatcher *outbox.Dispatcher, resolver pgdb.Resolver) *pipeline.Pipeline {
	return pipeline.New(logger, dispatcher, Projectors(resolver)...)
}

// NewWageringUseCase builds the provider operation use case.
func NewWageringUseCase(
	tx *uow.Manager,
	pipe *pipeline.Pipeline,
	cfg config.Config,
	logger *slog.Logger,
	m *metrics.Metrics,
	registerWager cqrs.CommandHandler[registerwageroperation.Command],
	awaitReference cqrs.CommandHandler[awaitwagerreference.Command],
	settleWager cqrs.CommandHandler[settlewageroperation.Command],
	moveWallet cqrs.CommandHandler[applywalletmovement.Command],
	wallets usecase.WalletReader,
	transactions usecase.TransactionReader,
	inboxRepo inbox.Repository,
) *usecase.WageringUseCase {
	return usecase.NewWageringUseCase(tx, pipe, cfg.Wagering, logger, m,
		registerWager, awaitReference, settleWager, moveWallet, wallets, transactions, inboxRepo)
}

// NewWalletsUseCase builds the wallet use case.
func NewWalletsUseCase(
	tx *uow.Manager,
	pipe *pipeline.Pipeline,
	cfg config.Config,
	logger *slog.Logger,
	m *metrics.Metrics,
	openWalletHandler cqrs.CommandHandler[openwallet.Command],
	registerWager cqrs.CommandHandler[registerwageroperation.Command],
	settleWager cqrs.CommandHandler[settlewageroperation.Command],
	wallets usecase.WalletReader,
) *usecase.WalletsUseCase {
	return usecase.NewWalletsUseCase(tx, pipe, cfg.Wagering, logger, m,
		openWalletHandler, registerWager, settleWager, wallets)
}

// NewLedgerUseCase builds the ledger and reconciliation use case.
func NewLedgerUseCase(
	tx *uow.Manager,
	ledger walletledgerentries.Repository,
	reconcile *walletreconciliation.Service,
	logger *slog.Logger,
	m *metrics.Metrics,
) *usecase.LedgerUseCase {
	return usecase.NewLedgerUseCase(tx, ledger, reconcile, logger, m)
}

// NewHandler builds the HTTP handler.
func NewHandler(
	wallets *usecase.WalletsUseCase,
	wagering *usecase.WageringUseCase,
	ledger *usecase.LedgerUseCase,
	queries *usecase.Queries,
	logger *slog.Logger,
) *httpapi.Handler {
	return httpapi.NewHandler(wallets, wagering, ledger, queries, logger)
}

// NewHealthService builds the liveness and readiness endpoints.
func NewHealthService(
	cfg config.Config,
	logger *slog.Logger,
	m *metrics.Metrics,
	pool *pgxpool.Pool,
	verifier *identity.Verifier,
	broker *sqs.Runtime,
) *health.Service {
	return health.New(cfg.InstanceID, logger, m,
		health.Probe{Name: "postgres", Check: func(ctx context.Context) error { return pgdb.Ping(ctx, pool) }},
		health.Probe{Name: "identity", Check: verifier.Ready},
		health.Probe{Name: "sqs", Check: broker.Client.Ready},
	)
}

// MetricsHandler is the Prometheus scrape endpoint. It has its own named type
// so the dependency graph cannot confuse it with any other http.Handler.
type MetricsHandler struct{ http.Handler }

// NewMetricsHandler exposes the Prometheus registry.
func NewMetricsHandler(m *metrics.Metrics) *MetricsHandler {
	return &MetricsHandler{Handler: promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{})}
}

// ServeHTTP starts the API listener under the Fx lifecycle.
//
// Shutdown is graceful and ordered: on SIGTERM the listener stops accepting,
// in-flight requests get the shutdown budget to finish, and only then do the
// dependencies this server used begin to close. Workers registered in the same
// application stop before the server does, because Fx runs stop hooks in
// reverse registration order.
func ServeHTTP(
	lc fx.Lifecycle,
	cfg config.Config,
	logger *slog.Logger,
	handler *httpapi.Handler,
	healthService *health.Service,
	metricsHandler *MetricsHandler,
	verifier *identity.Verifier,
) {
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			router := handler.Router(identity.Authenticate(verifier, logger), healthService.Handler(),
				metricsHandler, cfg.HTTP.MaxBodyBytes)
			server := &http.Server{
				Addr:              cfg.HTTP.Address,
				Handler:           router,
				ReadTimeout:       cfg.HTTP.ReadTimeout,
				ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
				WriteTimeout:      cfg.HTTP.WriteTimeout,
				IdleTimeout:       cfg.HTTP.IdleTimeout,
			}

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				logger.Info("http server listening", "address", cfg.HTTP.Address)
				if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					logger.Error("http server stopped unexpectedly", "error", err.Error())
				}
			}()

			lc.Append(fx.Hook{
				OnStop: func(ctx context.Context) error {
					// Stop accepting first, then let in-flight work finish.
					shutdownCtx, cancel := context.WithTimeout(ctx, cfg.HTTP.ShutdownBudget)
					defer cancel()
					if err := server.Shutdown(shutdownCtx); err != nil {
						logger.Warn("graceful shutdown exceeded its budget; closing",
							"error", err.Error())
						_ = server.Close()
					}
					wg.Wait()
					logger.Info("http server stopped")
					return nil
				},
			})
			return nil
		},
	})
}
