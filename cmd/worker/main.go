// Command worker runs the ingestion pipeline of the platform: the SQS consumer
// that processes provider operations, the outbox publisher that delivers the
// events this platform produces, and the resolver that retries reversals waiting
// for their reference.
//
// Several instances may run at once. They coordinate through the database —
// per-wallet locks, unique indexes and the inbox — not through each other, so
// adding an instance adds throughput without adding a consistency rule.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/ironledger/iron-ledger/internal/app/fxmodules"
)

const label = "worker"

func main() {
	var logger *slog.Logger

	app := fx.New(
		fx.WithLogger(func() fxevent.Logger { return fxevent.NopLogger }),
		fxmodules.PlatformModule,
		fxmodules.StorageModule,
		fxmodules.SlicesModule,
		fxmodules.MessagingModule,
		fxmodules.ApplicationModule,
		fxmodules.WorkersModule,
		// The worker exposes the same health probes and metrics as the API. A
		// process that cannot report its own health is impossible to operate.
		fx.Invoke(fxmodules.ServeHTTP),
		fx.Invoke(func(l *slog.Logger) { logger = l }),
	)

	startCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := app.Start(startCtx); err != nil {
		fatal(err, logger)
	}

	// Wait blocks until a signal arrives and every stop hook has returned, so
	// the process never exits while a worker still holds work.
	shutdown := app.Wait()
	<-shutdown

	logger.Info("worker stopped cleanly")
}

func fatal(err error, logger *slog.Logger) {
	if logger != nil {
		logger.Error(label+" failed to start", "error", err.Error())
	} else {
		_, _ = os.Stderr.WriteString(label + ": " + err.Error() + "\n")
	}
	os.Exit(1)
}
