package fxmodules

import (
	"context"
	"log/slog"
	"sync"

	"go.uber.org/fx"

	"github.com/ironledger/iron-ledger/internal/app/usecase"
	"github.com/ironledger/iron-ledger/internal/app/worker"
	"github.com/ironledger/iron-ledger/internal/messaging/outbox"
	"github.com/ironledger/iron-ledger/internal/messaging/sqs"
	"github.com/ironledger/iron-ledger/internal/platform/config"
	"github.com/ironledger/iron-ledger/internal/platform/logging"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
)

// WorkersModule provides and starts the long-running components.
//
// Every worker is a Run(ctx) loop registered as a lifecycle hook. Each one is
// given a context tied to the application rather than to start-up, and the stop
// hook cancels it and waits for the loop to return, which is what makes
// shutdown observable rather than hopeful: the process does not exit while a
// worker is still holding work.
var WorkersModule = fx.Module("workers",
	fx.Provide(NewConsumer),
	fx.Provide(NewPublisher),
	fx.Provide(NewReferenceResolver),
	fx.Invoke(StartConsumer),
	fx.Invoke(StartPublisher),
	fx.Invoke(StartReferenceResolver),
)

// NewConsumer builds the SQS consumer.
func NewConsumer(
	broker *sqs.Runtime,
	wagering *usecase.WageringUseCase,
	cfg config.Config,
	logger *slog.Logger,
	m *metrics.Metrics,
) *worker.Consumer {
	options := cfg.SQS
	options.QueueURL = broker.WagerQueue
	options.DLQURL = broker.WagerDeadLetterQueue
	return worker.NewConsumer(broker.Client, wagering, options, logger, m, cfg.InstanceID)
}

// NewPublisher builds the outbox publisher.
func NewPublisher(
	broker *sqs.Runtime,
	repo outbox.Repository,
	tx *uow.Manager,
	cfg config.Config,
	logger *slog.Logger,
	m *metrics.Metrics,
) *worker.Publisher {
	outboxCfg := cfg.Outbox
	outboxCfg.QueueURL = broker.EventsQueue
	return worker.NewPublisher(broker.Client, repo, tx, outboxCfg, broker.EventsQueue, logger, m, cfg.InstanceID)
}

// NewReferenceResolver builds the pending-reference resolver.
func NewReferenceResolver(
	wagering *usecase.WageringUseCase,
	cfg config.Config,
	logger *slog.Logger,
	m *metrics.Metrics,
) *worker.ReferenceResolver {
	return worker.NewReferenceResolver(wagering, cfg.Wagering, logger, m)
}

// run adapts a worker to the Fx lifecycle.
//
// The loop is given a context of its own, created here, rather than the one Fx
// hands to OnStart. That distinction is the whole reason this function exists.
// The start-up context is a child of the context passed to app.Start: it carries
// the start timeout and it is cancelled when the hook returns. A long-running
// loop given that context stops as soon as the start timeout elapses — sixty
// seconds, by default in cmd/worker — while the process itself stays up and
// keeps answering /health. The result is an instance that looks healthy, has
// published nothing for a minute, and is never restarted, because nothing is
// wrong as far as any probe can tell.
//
// OnStop cancels the context and then waits, so shutdown is observable: the
// process does not exit while a worker is still holding work.
func run(lc fx.Lifecycle, name string, logger *slog.Logger, loop func(context.Context) error) {
	ctx, cancel := context.WithCancel(context.Background())

	var done sync.WaitGroup

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			done.Add(1)
			go func() {
				defer done.Done()
				err := loop(ctx)
				switch {
				case err != nil && ctx.Err() == nil:
					// The worker failed while the process is still running. It
					// will not be restarted, so this is logged as an error and
					// not swallowed — but it is not made fatal here either,
					// because killing the process would stop the HTTP server
					// and turn a lost worker into an outage. worker_running
					// stays at 0, which is how an operator sees it.
					logger.Error("worker stopped while the process was running; it will not be restarted",
						logging.FieldComponent, name, "error", err.Error())
				case ctx.Err() == nil:
					// A loop that returns nil without being asked to stop has
					// finished its work. For a poll loop that means it is
					// broken, not idle.
					logger.Error("worker returned without being stopped",
						logging.FieldComponent, name)
				}
			}()
			return nil
		},
		OnStop: func(context.Context) error {
			cancel()
			done.Wait()
			return nil
		},
	})
}

// StartConsumer registers the SQS consumer.
func StartConsumer(lc fx.Lifecycle, consumer *worker.Consumer, logger *slog.Logger) {
	run(lc, "wager_consumer", logger.With(logging.FieldComponent, "wager_consumer"), consumer.Run)
}

// StartPublisher registers the outbox publisher.
func StartPublisher(lc fx.Lifecycle, publisher *worker.Publisher, logger *slog.Logger) {
	run(lc, "outbox_publisher", logger.With(logging.FieldComponent, "outbox_publisher"), publisher.Run)
}

// StartReferenceResolver registers the pending-reference resolver.
func StartReferenceResolver(lc fx.Lifecycle, resolver *worker.ReferenceResolver, logger *slog.Logger) {
	run(lc, "reference_resolver", logger.With(logging.FieldComponent, "reference_resolver"), resolver.Run)
}
