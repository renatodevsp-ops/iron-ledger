// Package integration holds the tests that need a real PostgreSQL. They are kept
// out of ./internal/... on purpose: `go test ./...` stays fast and hermetic,
// and this package is the one that proves the transaction, locking and
// constraint behaviour the unit tests can only approximate with fakes.
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/ironledger/ironledger/internal/platform/idgen"
	"github.com/ironledger/ironledger/internal/platform/postgres"
	"github.com/ironledger/ironledger/internal/usecase"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	testTenantID = "11111111-1111-4111-8111-111111111111"
	testActor    = "integration-test"

	image = "postgres:16-alpine"

	dbName     = "ironledger"
	dbUser     = "ironledger"
	dbPassword = "ironledger"
)

var (
	sharedPool *pgxpool.Pool
	sharedSvc  *usecase.Service

	// testDSN lets a test rebuild the whole stack from scratch, standing in for a
	// process restart.
	testDSN string
)

// TestMain starts one PostgreSQL for the whole package, runs the real migration
// chain against it and tears it down at the end. One container for the package
// keeps the suite fast; each test rolls its own data back or uses fresh ids.
func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := postgresContainer(ctx)
	if err != nil {
		return 0, fmt.Errorf("start postgres: %w", err)
	}
	defer testcontainers.TerminateContainer(container)

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return 0, fmt.Errorf("container dsn: %w", err)
	}
	testDSN = dsn

	pool, err := connectWithRetry(ctx, dsn)
	if err != nil {
		return 0, fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	sharedPool = pool

	// The real migration chain, run by the real runner. A broken migration fails
	// every test in the package, which is the point: schema drift must never be
	// discovered by a test that happened to touch the affected table.
	if _, err := postgres.Up(ctx, pool); err != nil {
		return 0, fmt.Errorf("migrate up: %w", err)
	}

	uow := postgres.NewUnitOfWork(pool, postgres.DefaultRetryPolicy(), nil)
	sharedSvc = usecase.New(uow, domain.SystemClock{}, idgen.New())

	return m.Run(), nil
}

func postgresContainer(ctx context.Context) (*tcpostgres.PostgresContainer, error) {
	return tcpostgres.Run(ctx,
		image,
		tcpostgres.WithDatabase(dbName),
		tcpostgres.WithUsername(dbUser),
		tcpostgres.WithPassword(dbPassword),
		// The server's default connection limit is small, and a concurrency test
		// that opens one connection per goroutine would exhaust it and report a
		// flaky failure instead of a real one.
		testcontainers.WithEnv(map[string]string{"POSTGRES_MAX_CONNECTIONS": "200"}),
		// Waiting for the port alone is not enough: PostgreSQL opens the socket
		// during its first startup pass and then restarts itself, so a client
		// connecting in that window gets SQLSTATE 57P03 "the database system is
		// starting up". This strategy waits for the readiness log twice before
		// trusting the port.
		tcpostgres.BasicWaitStrategies(),
		testcontainers.WithAdditionalWaitStrategyAndDeadline(
			3*time.Minute, wait.ForListeningPort("5432/tcp")),
	)
}

// connectWithRetry tolerates the brief window where the container accepts a TCP
// connection but the server is not yet answering queries. A test suite that dies
// on that would report a product failure for a test-harness timing detail.
func connectWithRetry(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	var lastErr error
	for range 20 {
		pool, err := postgres.Connect(ctx, postgres.DefaultPoolConfig(dsn))
		if err == nil {
			return pool, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("connect after retries: %w", lastErr)
}

// newWallet inserts a wallet directly, bypassing the API, so each test starts
// from a known balance. It uses the pool as the superuser the container was
// created with, which is also how the migration role is exercised.
func newWallet(t *testing.T, balance int64, currency domain.Currency, status domain.WalletStatus) string {
	t.Helper()
	ctx := context.Background()
	id := idgen.New().NewUUID()
	_, err := sharedPool.Exec(ctx, `
		INSERT INTO wallets (id, tenant_id, player_id, currency, balance_minor, status)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, testTenantID, "player-"+id[:8], string(currency), balance, string(status))
	require.NoError(t, err, "seed wallet")
	return id
}

// mustApply is for the happy paths, where an error is a test failure.
func mustApply(t *testing.T, walletID, key string, minor int64, currency domain.Currency) usecase.ApplyOutput {
	t.Helper()
	out, err := applyBet(t, walletID, key, minor, currency)
	require.NoError(t, err)
	return out
}

// applyBet returns the error so the rejection cases can assert on it.
func applyBet(t *testing.T, walletID, key string, minor int64, currency domain.Currency) (usecase.ApplyOutput, error) {
	t.Helper()
	return sharedSvc.Apply(context.Background(), usecase.ApplyInput{
		WalletID:       walletID,
		TenantID:       testTenantID,
		IdempotencyKey: key,
		Operation: domain.Operation{
			Type:          domain.OperationBET,
			Amount:        domain.Money{AmountMinor: minor, Currency: currency},
			TransactionID: "txn-" + key,
		},
		Channel: domain.ChannelAPI,
		Actor:   testActor,
	})
}

func balanceOf(t *testing.T, walletID string) int64 {
	t.Helper()
	var balance int64
	require.NoError(t, sharedPool.QueryRow(context.Background(),
		`SELECT balance_minor FROM wallets WHERE id = $1`, walletID).Scan(&balance))
	return balance
}

func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, sharedPool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

// postgres_PgErrorCode exposes the SQLSTATE so the tests can assert on the
// database's own guard rather than on a message string.
func postgres_PgErrorCode(err error) string {
	return postgres.PgErrorCode(err)
}
