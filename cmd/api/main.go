// Command api runs the HTTP surface of the platform.
//
// It serves the business API, the health probes and the metrics endpoint. It
// does not consume the queue: ingestion is the worker's job, so the API can be
// scaled and restarted without touching the delivery pipeline. Both processes
// share the same use cases, the same idempotency guarantees and the same
// database constraints.
//
// As a health check it probes its own liveness endpoint:
//
//	api -healthcheck
//
// which is what a container runtime needs in a distroless image, where there is
// no shell and no curl to run.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/ironledger/iron-ledger/internal/app/fxmodules"
)

const label = "api"

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the liveness endpoint and exit")
	flag.Parse()

	if *healthcheck {
		os.Exit(probe())
	}

	var logger *slog.Logger

	app := fx.New(
		fx.WithLogger(func() fxevent.Logger { return fxevent.NopLogger }),
		fxmodules.PlatformModule,
		fxmodules.StorageModule,
		fxmodules.SlicesModule,
		fxmodules.MessagingModule,
		fxmodules.ApplicationModule,
		fx.Invoke(fxmodules.ServeHTTP),
		fx.Invoke(func(l *slog.Logger) { logger = l }),
	)

	// Start returns once every hook has started. Wait then blocks until a
	// signal arrives and every stop hook has finished — in reverse order, so the
	// HTTP server drains before the pool it used is closed.
	startCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := app.Start(startCtx); err != nil {
		fatal(err, logger)
	}

	shutdown := app.Wait()
	<-shutdown

	logger.Info("api stopped cleanly")
}

// probe checks the liveness endpoint of a running instance.
func probe() int {
	address := os.Getenv("HTTP_ADDRESS")
	if address == "" {
		address = ":8080"
	}
	if address[0] == ':' {
		address = "127.0.0.1" + address
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/health/live", nil)
	if err != nil {
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func fatal(err error, logger *slog.Logger) {
	if logger != nil {
		logger.Error(label+" failed to start", "error", err.Error())
	} else {
		_, _ = os.Stderr.WriteString(label + ": " + err.Error() + "\n")
	}
	os.Exit(1)
}
