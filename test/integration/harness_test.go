//go:build integration

// Package integration exercises the whole platform against the real
// dependencies: a real PostgreSQL, a real Keycloak and a real SQS.
//
// Nothing here is mocked. The point of these tests is precisely the part a mock
// cannot show — that the constraints, the locks, the unique indexes and the
// broker's FIFO semantics actually hold when three independent processes write
// to the same wallet at once.
//
// Run with:
//
//	go test -tags integration -race ./test/integration/...
//
// with the dependencies reachable at the addresses in TEST_* below (see
// README.md and `make integration`).
package integration

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	cqrs "github.com/terraskye/eventsourcing"

	"github.com/ironledger/iron-ledger/internal/app/fxmodules"
	"github.com/ironledger/iron-ledger/internal/app/pipeline"
	"github.com/ironledger/iron-ledger/internal/app/usecase"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
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
	"github.com/ironledger/iron-ledger/internal/platform/logging"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
	"github.com/ironledger/iron-ledger/internal/platform/migrations"
	"github.com/ironledger/iron-ledger/internal/platform/pgdb"
	"github.com/ironledger/iron-ledger/internal/platform/pgevents"
	"github.com/ironledger/iron-ledger/internal/platform/support"
	"github.com/ironledger/iron-ledger/internal/platform/uow"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/ids"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/money"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

var (
	_ = http.StatusOK
	_ = json.Marshal
	_ = strings.TrimSpace
	_ = ids.NewToken
	_ = sqs.WagerQueueName

	// testConfig is read once from the environment. Every value has a default
	// that matches docker compose, so `make integration` works with no setup.
	testConfig = func() config.Config {
		cfg, err := config.Load()
		if err != nil {
			panic(err)
		}
		cfg.Database.Migrations = false
		cfg.Wagering.ReferenceBackoffBase = 100 * time.Millisecond
		cfg.Wagering.ReferenceBackoffMax = 400 * time.Millisecond
		cfg.Wagering.MaxReferenceAttempts = 4
		cfg.Wagering.MaxConflictRetries = 12
		return cfg
	}()

	logger = logging.New("iron-ledger-test", "test", "warn")
)

// instance is one independent participant: its own connection pool, its own
// event store, its own use cases. Three of them is what proves the guarantees
// do not depend on a single process.
type instance struct {
	name    string
	pool    *pgxpool.Pool
	db      pgdb.Resolver
	store   cqrs.EventStore
	tx      *uow.Manager
	pipe    *pipeline.Pipeline
	wager   *usecase.WageringUseCase
	wallets *usecase.WalletsUseCase
	ledger  *usecase.LedgerUseCase
	queries *usecase.Queries
	metrics *metrics.Metrics
}

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := ensureSchema(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "integration: cannot prepare the database: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// migrationsDir locates the migration files by walking up from the working
// directory, so the suite runs from anywhere inside the repository.
func migrationsDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return "migrations"
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "migrations")); err == nil {
			return filepath.Join(dir, "migrations")
		}
		dir = filepath.Dir(dir)
	}
	return "migrations"
}

// ensureSchema applies pending migrations and truncates the financial tables, so
// a test run starts from a known state and still proves the migrations apply.
func ensureSchema(ctx context.Context) error {
	db, err := sql.Open("pgx", testConfig.Database.DSN)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	runner, err := migrations.New(db, migrationsDir())
	if err != nil {
		return err
	}
	defer runner.Close()

	if err := runner.Up(); err != nil {
		return err
	}

	// The ledger is append-only down to TRUNCATE, which is exactly right in
	// production and inconvenient for a test fixture. Suspending the guards just
	// long enough to empty the tables is the only way to reset; the guarantee
	// itself is asserted by Test_theLedgerIsAppendOnly, which runs with them on.
	const suspendLedgerGuards = `ALTER TABLE wallet_ledger_entries DISABLE TRIGGER wallet_ledger_entries_no_truncate`
	if _, err := db.ExecContext(ctx, suspendLedgerGuards); err != nil {
		return fmt.Errorf("suspend ledger guards: %w", err)
	}

	truncate := `
		TRUNCATE outbox_messages, inbox_messages, wager_transactions,
		         wallet_ledger_entries, wallets, events RESTART IDENTITY CASCADE`
	if _, err := db.ExecContext(ctx, truncate); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}

	const restoreLedgerGuards = `ALTER TABLE wallet_ledger_entries ENABLE TRIGGER wallet_ledger_entries_no_truncate`
	if _, err := db.ExecContext(ctx, restoreLedgerGuards); err != nil {
		return fmt.Errorf("restore ledger guards: %w", err)
	}
	return nil
}

// newInstance builds a participant. Each one owns its own pool, so a test that
// runs three of them is running three processes' worth of database state.
func newInstance(t *testing.T, name string) *instance {
	t.Helper()

	ctx := context.Background()
	pool, err := pgdb.NewPool(ctx, testConfig.Database)
	if err != nil {
		t.Fatalf("instance %s: connect: %v", name, err)
	}
	t.Cleanup(pool.Close)

	db := pgdb.NewResolver(pool)
	store := pgevents.New(pool)
	m := metrics.New()
	tx := uow.NewManager(pool, logger, m, testConfig.Wagering.MaxConflictRetries)

	dispatcher := outbox.NewDispatcher(outbox.NewRepo(db), integrationBuilders())
	pipe := pipeline.New(logger, dispatcher, fxmodules.Projectors(db)...)

	extractors := support.MetadataExtractors{support.OperationalMetadata()}
	walletsRepo := walletdetails.NewRepo(db)
	ledgerRepo := walletledgerentries.NewRepo(db)
	transactionsRepo := wagertransactiondetails.NewRepo(db)
	inboxRepo := inbox.NewRepo(db)

	reconcile := walletreconciliation.New(
		ledgerRepo,
		walletreconciliation.NewWalletAdapter(walletsRepo),
		usecase.ReconciliationObserver{Metrics: m},
	)

	wager := usecase.NewWageringUseCase(tx, pipe, testConfig.Wagering, logger, m,
		registerwageroperation.NewCommandHandler(store, extractors...),
		awaitwagerreference.NewCommandHandler(store, extractors...),
		settlewageroperation.NewCommandHandler(store, extractors...),
		applywalletmovement.NewCommandHandler(store, extractors...),
		walletsRepo, transactionsRepo, inboxRepo,
	)

	wallets := usecase.NewWalletsUseCase(tx, pipe, testConfig.Wagering, logger, m,
		openwallet.NewCommandHandler(store, extractors...),
		registerwageroperation.NewCommandHandler(store, extractors...),
		settlewageroperation.NewCommandHandler(store, extractors...),
		walletsRepo,
	)

	return &instance{
		name:    name,
		pool:    pool,
		db:      db,
		store:   store,
		tx:      tx,
		pipe:    pipe,
		wager:   wager,
		wallets: wallets,
		ledger:  usecase.NewLedgerUseCase(tx, ledgerRepo, reconcile, logger, m),
		queries: usecase.NewQueries(transactionsRepo),
		metrics: m,
	}
}

// integrationBuilders composes the integration builders of every bounded
// context, exactly as the application does at start-up.
func integrationBuilders() integration.MultiBuilder {
	return integration.MultiBuilder{
		walletevents.NewBuilder(),
		wageringevents.NewBuilder(),
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (i *instance) openWallet(t *testing.T, playerID uuid.UUID, amount string) uuid.UUID {
	t.Helper()
	// The correlation id matters here: in production an opening arrives over
	// HTTP and carries one, and the events it appends stamp it. A fixture that
	// opened a wallet without one would publish an envelope the real surface
	// never produces.
	ctx := logging.WithCorrelationID(context.Background(), "")
	view, err := i.wallets.Open(ctx, usecase.OpenWalletRequest{
		PlayerID: playerID,
		Initial:  money.MustParse(amount, "BRL"),
	})
	if err != nil {
		t.Fatalf("open wallet: %v", err)
	}
	id, err := uuid.Parse(view.ID)
	if err != nil {
		t.Fatalf("open wallet: %v", err)
	}
	return id
}

// scope namespaces the identities a test uses, so two tests can use the same
// readable external ids without colliding in the persistent index.
type scope string

func scopeOf(t *testing.T) scope {
	t.Helper()
	return scope(strings.ReplaceAll(t.Name(), "/", "-") + "-")
}

func (s scope) id(value string) string { return string(s) + value }

type operation struct {
	scope      scope
	provider   string
	externalID string
	playerID   uuid.UUID
	walletID   uuid.UUID
	round      string
	game       string
	kind       string
	amount     string
	reference  string
	source     usecase.Source
	inbox      *usecase.InboxContext
}

func (o operation) request(t *testing.T) usecase.OperationRequest {
	t.Helper()
	req := usecase.OperationRequest{
		IdempotencyKey:        o.provider + ":" + o.scope.id(o.externalID),
		ProviderID:            o.provider,
		ExternalTransactionID: o.scope.id(o.externalID),
		PlayerID:              o.playerID,
		WalletID:              o.walletID,
		RoundID:               firstNonEmpty(o.round, "round-1"),
		GameID:                firstNonEmpty(o.game, "fortune-chimp"),
		Kind:                  o.kind,
		Amount:                o.amount,
		Currency:              "BRL",
		Source:                o.source,
		Inbox:                 o.inbox,
	}
	if o.reference != "" {
		reference := o.scope.id(o.reference)
		req.ReferenceExternalID = &reference
	}
	return req
}

func (i *instance) submit(t *testing.T, op operation) (*usecase.OperationResult, error) {
	t.Helper()
	if op.scope == "" {
		op.scope = scopeOf(t)
	}
	ctx := logging.WithCorrelationID(context.Background(), "")
	return i.wager.Submit(ctx, op.request(t))
}

func (i *instance) balance(t *testing.T, walletID uuid.UUID) money.Money {
	t.Helper()
	view, err := i.wallets.Get(context.Background(), walletID)
	if err != nil {
		t.Fatalf("read wallet: %v", err)
	}
	return money.MustParse(view.Balance.Amount, money.Currency(view.Balance.Currency))
}

func (i *instance) reconcile(t *testing.T, walletID uuid.UUID) *walletreconciliation.Report {
	t.Helper()
	report, err := i.ledger.Reconcile(context.Background(), walletID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return report
}

// ledgerEntries counts the entries of a wallet.
func (i *instance) ledgerEntries(t *testing.T, walletID uuid.UUID) int {
	t.Helper()
	page, err := i.ledger.List(context.Background(), walletID, "", 200)
	if err != nil {
		t.Fatalf("list ledger: %v", err)
	}
	return len(page.Data)
}

// countDebits counts the DEBIT entries of a wallet.
func (i *instance) countDebits(t *testing.T, walletID uuid.UUID) int {
	t.Helper()
	page, err := i.ledger.List(context.Background(), walletID, "", 200)
	if err != nil {
		t.Fatalf("list ledger: %v", err)
	}
	count := 0
	for _, entry := range page.Data {
		if entry.Direction == "DEBIT" {
			count++
		}
	}
	return count
}

func requireStatus(t *testing.T, result *usecase.OperationResult, err error, want string) {
	t.Helper()
	if err != nil {
		t.Fatalf("operation failed: %v", err)
	}
	if result.Status != want {
		t.Fatalf("status = %s (code %s), want %s", result.Status, result.FailureCode, want)
	}
}

func requireCode(t *testing.T, err error, want xerr.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected rejection %s, got success", want)
	}
	code, ok := xerr.CodeOf(err)
	if !ok {
		t.Fatalf("error %v carries no failure code", err)
	}
	if code != want {
		t.Fatalf("failure code = %s, want %s", code, want)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// countRows is a small escape hatch for assertions about the schema itself.
func (i *instance) countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var count int
	if err := i.pool.QueryRow(context.Background(), sql, args...).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

func httpStatus(t *testing.T, err error) int {
	t.Helper()
	rejection, ok := xerr.As(err)
	if !ok {
		t.Fatalf("error %v is not a rejection", err)
	}
	return rejection.Kind.HTTPStatus()
}

// nowUTC is the current instant, used where a test only needs a plausible time.
func nowUTC() time.Time { return time.Now().UTC() }

// resolverDue lists the reversals waiting for a reference whose next attempt is
// due, the same query the resolver worker runs. The suite shares one database,
// so the caller narrows the result to the transactions it cares about.
func resolverDue(t *testing.T, instance *instance, only ...uuid.UUID) []*wagertransactiondetails.Entity {
	t.Helper()
	due, err := instance.wager.DuePending(context.Background(), 100)
	if err != nil {
		t.Fatalf("list pending references: %v", err)
	}
	if len(only) == 0 {
		return due
	}
	wanted := make(map[uuid.UUID]bool, len(only))
	for _, id := range only {
		wanted[id] = true
	}
	filtered := make([]*wagertransactiondetails.Entity, 0, len(only))
	for _, entity := range due {
		if wanted[entity.ID] {
			filtered = append(filtered, entity)
		}
	}
	return filtered
}

// resolverStatus reads the status a transaction ended up with.
func resolverStatus(t *testing.T, instance *instance, transactionID uuid.UUID) string {
	t.Helper()
	view, err := instance.queries.GetTransaction(context.Background(), transactionID)
	if err != nil {
		t.Fatalf("read transaction: %v", err)
	}
	return view.Status
}

// resolverSweep runs one sweep of the resolver worker and reports the status of
// the transaction it was waiting on.
func resolverSweep(t *testing.T, resolver, observer *instance, transactionID ...uuid.UUID) string {
	t.Helper()
	due := resolverDue(t, observer, transactionID...)
	for _, pending := range due {
		resolver.resume(t, pending)
		if len(transactionID) > 0 && pending.ID == transactionID[0] {
			return resolverStatus(t, observer, pending.ID)
		}
	}
	return ""
}

// resume re-drives a pending transaction, as the reference worker does.
func (i *instance) resume(t *testing.T, entity *wagertransactiondetails.Entity) {
	t.Helper()
	if _, err := i.wager.Resume(context.Background(), entity); err != nil {
		t.Fatalf("resume %s: %v", entity.ID, err)
	}
}

// httpStatusOf is a readability helper for assertions about the accepted state.
func httpStatusOf(result *usecase.OperationResult) int {
	if result.Status == string(domain.StatusPendingReference) {
		return 202
	}
	return 200
}

// resolverDueAfter waits for the scheduled attempt to become due, which is what
// the resolver worker does on its own ticker.
func resolverDueAfter(t *testing.T, instance *instance, backoff time.Duration, only ...uuid.UUID) []*wagertransactiondetails.Entity {
	t.Helper()
	time.Sleep(backoff + 20*time.Millisecond)
	return resolverDue(t, instance, only...)
}

// postgresCode extracts the SQLSTATE of a PostgreSQL error, or "".
func postgresCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// openRequest builds an opening request for a player, for the assertions that
// call the use case directly.
func openRequest(playerID uuid.UUID, amount string) usecase.OpenWalletRequest {
	return usecase.OpenWalletRequest{
		PlayerID: playerID,
		Initial:  money.MustParse(amount, "BRL"),
	}
}

// storedEventIDs reads every identity the outbox holds, published or not, so a
// test can prove a published event was committed before it was published.
func (i *instance) storedEventIDs(t *testing.T) map[uuid.UUID]bool {
	t.Helper()
	rows, err := i.pool.Query(context.Background(),
		`SELECT event_id FROM outbox_messages`)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()

	ids := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan the outbox: %v", err)
		}
		ids[id] = true
	}
	return ids
}

// maxAttempts returns the highest publication attempt count recorded.
func (i *instance) maxAttempts(t *testing.T) int {
	t.Helper()
	var attempts int
	if err := i.pool.QueryRow(context.Background(),
		`SELECT COALESCE(max(attempts), 0) FROM outbox_messages`).Scan(&attempts); err != nil {
		t.Fatalf("read the attempt count: %v", err)
	}
	return attempts
}

// waitFor polls until condition holds, so the suite never depends on a fixed
// sleep to observe work that is happening concurrently.
func waitFor(t *testing.T, budget time.Duration, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", budget, what)
}

// encodeSegment renders a token segment.
func encodeSegment(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeSegment reads a token segment into out.
func decodeSegment(segment []byte, out any) error {
	decoded, err := base64.RawURLEncoding.DecodeString(string(segment))
	if err != nil {
		return err
	}
	return json.Unmarshal(decoded, out)
}
