// Package fxmodules is the Uber Fx composition of the platform.
//
// The modules mirror the bounded contexts and the layers above them, so the
// dependency graph can be read top to bottom: platform infrastructure, storage,
// slices, messaging, application, then the runnable components. Every
// long-running component is owned by an fx.Lifecycle hook rather than by a
// goroutine someone forgot to stop, and dependencies are closed only after the
// components that use them have finished.
package fxmodules

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"go.uber.org/fx"

	"github.com/jackc/pgx/v5/pgxpool"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/identity"
	"github.com/ironledger/iron-ledger/internal/platform/config"
	"github.com/ironledger/iron-ledger/internal/platform/logging"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
	"github.com/ironledger/iron-ledger/internal/platform/migrations"
	"github.com/ironledger/iron-ledger/internal/platform/pgdb"
	"github.com/ironledger/iron-ledger/internal/platform/pgevents"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
)

// PlatformModule provides configuration, logging, metrics and the database.
var PlatformModule = fx.Module("platform",
	fx.Provide(NewConfig),
	fx.Provide(NewLogger),
	fx.Provide(metrics.New),
	fx.Provide(NewPool),
	fx.Provide(NewResolver),
	fx.Provide(NewEventStore),
	fx.Provide(NewUnitOfWork),
	fx.Provide(NewIdentityVerifier),
)

// NewConfig loads and validates the configuration.
func NewConfig() (config.Config, error) { return config.Load() }

// NewLogger builds the root structured logger.
func NewLogger(cfg config.Config) *slog.Logger { return newLogger(cfg) }

// NewPool builds the PostgreSQL pool, optionally applying pending migrations.
func NewPool(lc fx.Lifecycle, cfg config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgdb.NewPool(ctx, cfg.Database)
	if err != nil {
		return nil, err
	}

	if cfg.Database.Migrations {
		if err := applyMigrations(ctx, cfg, pool, logger); err != nil {
			pool.Close()
			return nil, err
		}
	}

	// The pool is closed last: Fx runs stop hooks in reverse order, so the HTTP
	// server and the workers, which were started after it, have already stopped
	// by the time this runs.
	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			pool.Close()
			return nil
		},
	})
	return pool, nil
}

func applyMigrations(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, logger *slog.Logger) error {
	db, err := sql.Open("pgx", cfg.Database.DSN)
	if err != nil {
		return fmt.Errorf("migrations: open: %w", err)
	}
	defer db.Close()

	runner, err := migrations.New(db, MigrationsDir)
	if err != nil {
		return err
	}
	defer runner.Close()

	if err := runner.Up(); err != nil {
		return err
	}
	version, dirty, err := runner.Version()
	if err != nil {
		return err
	}
	logger.Info("schema is up to date", "version", version, "dirty", dirty)
	return nil
}

// NewResolver builds the query resolver repositories use.
func NewResolver(pool *pgxpool.Pool) pgdb.Resolver { return pgdb.NewResolver(pool) }

// NewEventStore builds the transaction-aware event store.
func NewEventStore(pool *pgxpool.Pool) cqrs.EventStore { return pgevents.New(pool) }

// NewUnitOfWork builds the transaction manager.
func NewUnitOfWork(cfg config.Config, pool *pgxpool.Pool, logger *slog.Logger, m *metrics.Metrics) *uow.Manager {
	return uow.NewManager(pool, logger, m, cfg.Wagering.MaxConflictRetries)
}

// NewIdentityVerifier discovers the identity provider and prepares token
// verification.
func NewIdentityVerifier(cfg config.Config) (*identity.Verifier, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return identity.NewVerifier(ctx, cfg.Auth)
}

// MigrationsDir is where the schema files live, relative to the working
// directory of the binaries.
var MigrationsDir = "migrations"

// newLogger builds the root structured logger.
func newLogger(cfg config.Config) *slog.Logger {
	return logging.New(cfg.ServiceName, cfg.Environment, cfg.LogLevel)
}
