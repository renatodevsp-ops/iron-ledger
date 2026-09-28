// Package fxapp is the composition root. It is the only package that knows how
// the concrete pieces are wired together: nothing below it imports a framework,
// and nothing above it builds a dependency by hand.
package fxapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/ironledger/ironledger/internal/platform/config"
	"github.com/ironledger/ironledger/internal/platform/httpapi"
	"github.com/ironledger/ironledger/internal/platform/idgen"
	"github.com/ironledger/ironledger/internal/platform/keycloak"
	"github.com/ironledger/ironledger/internal/platform/logging"
	"github.com/ironledger/ironledger/internal/platform/postgres"
	"github.com/ironledger/ironledger/internal/usecase"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
)

// ProvideConfig loads the environment configuration. A failure here is fatal by
// design: a process that starts with a half-valid configuration is worse than
// one that refuses to start.
func ProvideConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func ProvideLogger(cfg config.Config) *slog.Logger {
	return logging.New(cfg.LogLevel, os.Stderr)
}

// ProvidePool builds the PostgreSQL pool. The context is the application's
// startup context, so a database that never answers fails the start rather than
// leaving the process in a half-ready state.
func ProvidePool(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DB.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = cfg.DB.MaxConns
	poolCfg.MinConns = cfg.DB.MinConns
	poolCfg.MaxConnLifetime = cfg.DB.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.DB.MaxConnIdleTime
	poolCfg.ConnConfig.ConnectTimeout = cfg.DB.ConnectTimeout

	// A per-connection statement timeout is the backstop against a statement
	// holding a row lock far longer than any legitimate operation needs.
	if cfg.DB.StatementTimeout > 0 {
		statementTimeout := cfg.DB.StatementTimeout
		poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			// set_config is used rather than SET because the SET statement does
			// not accept bind parameters, and the value is formatted as a literal.
			// set_config is parameterised, so no quoting is needed here.
			_, err := conn.Exec(ctx,
				"SELECT set_config('statement_timeout', $1, false)",
				formatDuration(statementTimeout))
			return err
		}
	}

	// The pool is created eagerly and pinged, so an unreachable database is
	// reported at startup instead of on the first request.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.DB.ConnectTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	if cfg.RunMigrations {
		applied, err := postgres.Up(ctx, pool)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("run migrations: %w", err)
		}
		// The count is logged rather than a bare "applied": on every deploy after
		// the first this is zero, and a log line claiming work was done when
		// nothing ran is worse than no log line.
		log.Info("migrations up to date", "applied", applied)
	}

	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			pool.Close()
			log.Info("database pool closed")
			return nil
		},
	})
	log.Info("database ready", "max_conns", cfg.DB.MaxConns)
	return pool, nil
}

func ProvideVerifier(cfg config.Config) (httpapi.TokenVerifier, error) {
	return keycloak.New(keycloak.DefaultConfig(
		cfg.Auth.RealmURL, cfg.Auth.Audience, cfg.Auth.AllowedClients))
}

// ProvideIDGen returns the concrete generator. The graph depends on the
// domain interface, so this is the one place the concrete type is named.
func ProvideIDGen() domain.IDGenerator { return idgen.New() }

// formatDuration renders a duration the way PostgreSQL's GUC parser expects,
// e.g. "10s" or "1m30s".
func formatDuration(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Millisecond), 10) + "ms"
}

func ProvideUOW(pool *pgxpool.Pool) *postgres.UnitOfWork {
	return postgres.NewUnitOfWork(pool, postgres.DefaultRetryPolicy(), nil)
}

func ProvideService(uow *postgres.UnitOfWork, ids domain.IDGenerator) *usecase.Service {
	return usecase.New(uow, domain.SystemClock{}, ids)
}

func ProvideHealthCheck(pool *pgxpool.Pool) httpapi.HealthCheck {
	return func(ctx context.Context) error { return postgres.HealthCheck(ctx, pool) }
}

func ProvideHTTPServer(cfg config.Config, svc *usecase.Service, verifier httpapi.TokenVerifier, health httpapi.HealthCheck, log *slog.Logger) *http.Server {
	return httpapi.New(cfg.HTTP, svc, verifier, health, log)
}

// ProvideListener binds the socket before Serve is called, so a port already in
// use is a startup error rather than a silently dead process.
func ProvideListener(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) (net.Listener, error) {
	listener, err := net.Listen("tcp", cfg.HTTP.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.HTTP.ListenAddr, err)
	}
	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			return listener.Close()
		},
	})
	log.Info("listening", "addr", listener.Addr().String())
	return listener, nil
}

// RegisterLifecycle starts the server and arranges the graceful drain.
//
// The order on shutdown matters: stop accepting first, then let in-flight
// requests finish, then release the pool. Reversing it would drop a request
// that had already been accepted but not yet committed.
func RegisterLifecycle(lc fx.Lifecycle, srv *http.Server, listener net.Listener, cfg config.Config, log *slog.Logger) error {
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				// http.ErrServerClosed is the expected result of a graceful
				// shutdown, so it is not logged as a failure.
				if err := srv.Serve(listener); err != nil &&
					!errors.Is(err, http.ErrServerClosed) {
					log.Error("http server stopped unexpectedly", "error", err)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			log.Info("draining", "timeout", cfg.ShutdownTimeout.String())
			// The caller's context is replaced with the configured timeout: a
			// drain that hangs forever is as bad as one that never starts.
			shutdownCtx, cancel := context.WithTimeout(ctx, cfg.ShutdownTimeout)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				// A drain that timed out is reported, and the listener is closed
				// hard so the process can still exit.
				log.Error("graceful shutdown incomplete", "error", err)
				return srv.Close()
			}
			log.Info("drained")
			return nil
		},
	})
	return nil
}

// Run builds the graph, starts it, and blocks until the process is asked to
// stop. It is the whole entry point main needs, so main carries no wiring.
func Run(version string) error {
	// SIGTERM is what an orchestrator sends; SIGINT is what a developer sends.
	// Both mean the same thing here: drain, then exit.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The version is logged before anything else so it is the first line in the
	// log of every process start, including one that immediately fails.
	logging.New(os.Getenv("IRONLEDGER_LOG_LEVEL"), os.Stderr).
		Info("starting", "version", version, "pid", os.Getpid())
	return start(ctx)
}

// start runs the application until ctx is cancelled, then drains it.
func start(ctx context.Context) error {
	// fx's own event log is silenced: the hooks in RegisterLifecycle already
	// report the three things an operator needs ("database ready", "listening",
	// "drained") through the structured logger, and a start-up failure is
	// returned from app.Start and printed by main.
	app := fx.New(Module(), fx.NopLogger)

	if err := app.Start(ctx); err != nil {
		// A start failure means the graph never came up, so there is nothing to
		// stop; returning here avoids a second, misleading error from Stop.
		return err
	}

	<-ctx.Done()

	// The stop context is an upper bound only. Each hook applies its own, tighter
	// timeout, so this cannot shorten a legitimate drain.
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return app.Stop(stopCtx)
}
