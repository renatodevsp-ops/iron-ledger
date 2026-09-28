package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/ironledger/ironledger/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockKey is the advisory lock key the runner holds for the duration of
// a run. It is scoped to the migration table: it serializes schema changes
// between instances of this service and nothing else. No business transaction
// ever takes it, so per-wallet concurrency is untouched (Constitution VII,
// research.md D-7 rejected pg_advisory_xact_lock for exactly this reason).
const migrationLockKey int64 = 8_247_110_312_884_162_306

const createSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	name       TEXT        PRIMARY KEY,
	checksum   TEXT        NOT NULL,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

type migration struct {
	name    string // 001_extensions
	up      string
	down    string
	sum     string // sha256 of up
	hasDown bool
}

func (m migration) String() string { return m.name }

// Up applies every pending migration in order and returns how many ran.
// Applying an already-applied migration is a no-op, so the command is
// idempotent, and a file whose contents changed after it was applied is a hard
// error rather than a silent divergence.
func Up(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return 0, fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		// Best effort: the session ends on release anyway, which drops the lock.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, createSchemaMigrations); err != nil {
		return 0, fmt.Errorf("create schema_migrations: %w", err)
	}

	all, err := loadMigrations()
	if err != nil {
		return 0, err
	}

	applied, err := appliedChecksums(ctx, conn.Conn())
	if err != nil {
		return 0, err
	}

	// Refuse to continue if an applied file was edited in place: a checksum
	// drift means the database and the repository disagree about the schema.
	index := indexByName(all)
	for name, sum := range applied {
		m, ok := index[name]
		if !ok {
			return 0, fmt.Errorf("migration %s is applied but no longer present in the repository", name)
		}
		if m.sum != sum {
			return 0, fmt.Errorf("migration %s was modified after it was applied (recorded %s, file %s); write a new migration instead", name, sum, m.sum)
		}
	}

	ran := 0
	for _, m := range all {
		if _, done := applied[m.name]; done {
			continue
		}
		if err := applyOne(ctx, conn.Conn(), m); err != nil {
			return ran, err
		}
		ran++
	}
	return ran, nil
}

// Down rolls back the given number of most recently applied migrations, one at
// a time, in reverse order.
func Down(ctx context.Context, pool *pgxpool.Pool, steps int) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, createSchemaMigrations); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	all, err := loadMigrations()
	if err != nil {
		return err
	}

	rows, err := conn.Query(ctx, `SELECT name FROM schema_migrations ORDER BY applied_at DESC, name DESC LIMIT $1`, steps)
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("collect applied migrations: %w", err)
	}

	index := indexByName(all)
	for _, name := range names {
		m, ok := index[name]
		if !ok {
			return fmt.Errorf("migration %s is applied but no longer present in the repository", name)
		}
		if !m.hasDown {
			return fmt.Errorf("migration %s has no .down.sql and cannot be rolled back", m.name)
		}
		if err := rollbackOne(ctx, conn.Conn(), m); err != nil {
			return err
		}
	}
	return nil
}

// Applied lists the names of the migrations currently recorded as applied.
func Applied(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func indexByName(all []migration) map[string]migration {
	out := make(map[string]migration, len(all))
	for _, m := range all {
		out[m.name] = m
	}
	return out
}

func applyOne(ctx context.Context, conn *pgx.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, m.up); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (name, checksum) VALUES ($1, $2)`,
		m.name, m.sum); err != nil {
		return fmt.Errorf("record migration %s: %w", m.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.name, err)
	}
	return nil
}

func rollbackOne(ctx context.Context, conn *pgx.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin rollback %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, m.down); err != nil {
		return fmt.Errorf("rollback migration %s: %w", m.name, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE name = $1`, m.name); err != nil {
		return fmt.Errorf("remove migration record %s: %w", m.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rollback %s: %w", m.name, err)
	}
	return nil
}

func appliedChecksums(ctx context.Context, conn *pgx.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, `SELECT name, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	// RowToStructByName is required rather than RowTo: RowTo hands Scan a single
	// struct destination, which pgx cannot decompose into the row's columns, and
	// the failure surfaces as "number of field descriptions must equal number of
	// destinations". The db tags bind the fields to the lower-case column names.
	pairs, err := pgx.CollectRows(rows, pgx.RowToStructByName[struct {
		Name     string `db:"name"`
		Checksum string `db:"checksum"`
	}])
	if err != nil {
		return nil, fmt.Errorf("collect applied migrations: %w", err)
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		out[p.Name] = p.Checksum
	}
	return out, nil
}

// AppliedMigration is one migration as recorded in the database.
type AppliedMigration struct {
	Name     string
	Checksum string
}

// EmbeddedMigrations returns the migration set compiled into this binary, in
// order, with the checksum each file currently has. It lets an operator compare
// what the repository believes against what the database recorded, which is the
// only way to diagnose a drift without parsing SQL by hand.
func EmbeddedMigrations() ([]AppliedMigration, error) {
	all, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	out := make([]AppliedMigration, 0, len(all))
	for _, m := range all {
		out = append(out, AppliedMigration{Name: m.name, Checksum: m.sum})
	}
	return out, nil
}

// loadMigrations reads the embedded files and pairs each NNN_name.up.sql with
// its NNN_name.down.sql. The numeric prefix defines the order, and a duplicate
// prefix is an error rather than a silent last-wins.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	byName := map[string]*migration{}
	order := []string{}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := migrations.FS.ReadFile(e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		prefix, name, up, err := splitMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		if prefix < 0 {
			return nil, fmt.Errorf("migration %s must start with a numeric prefix", e.Name())
		}
		m, ok := byName[name]
		if !ok {
			m = &migration{name: name}
			byName[name] = m
			order = append(order, name)
		}
		sum := sha256.Sum256(body)
		if up {
			m.up = string(body)
			m.sum = hex.EncodeToString(sum[:])
		} else {
			m.down = string(body)
			m.hasDown = true
		}
	}

	out := make([]migration, 0, len(order))
	for _, name := range order {
		m := byName[name]
		if m.up == "" {
			return nil, fmt.Errorf("migration %s has a .down.sql but no .up.sql", name)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// splitMigrationName parses NNN_name.up.sql / NNN_name.down.sql. It returns
// the numeric ordering prefix, the migration name (the file base without the
// direction suffix) and which direction the file is.
func splitMigrationName(file string) (prefix int, name string, up bool, err error) {
	base := strings.TrimSuffix(file, ".sql")
	switch {
	case strings.HasSuffix(base, ".up"):
		up = true
		base = strings.TrimSuffix(base, ".up")
	case strings.HasSuffix(base, ".down"):
		base = strings.TrimSuffix(base, ".down")
	default:
		return 0, "", false, fmt.Errorf("migration %s must end in .up.sql or .down.sql", file)
	}
	head, rest, ok := strings.Cut(base, "_")
	if !ok || rest == "" {
		return 0, "", false, fmt.Errorf("migration %s must be named NNN_name.up.sql", file)
	}
	prefix, err = strconv.Atoi(head)
	if err != nil {
		return 0, "", false, fmt.Errorf("migration %s has a non-numeric prefix %q", file, head)
	}
	return prefix, base, up, nil
}
