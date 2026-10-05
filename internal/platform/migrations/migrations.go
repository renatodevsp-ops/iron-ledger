// Package migrations applies and rolls back the versioned schema.
//
// Migrations run as plain SQL files named {version}_{name}.{up,down}.sql, which
// keeps the schema reviewable in a diff and keeps the application free of any
// schema-mutating dependency at request time.
package migrations

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	// pgx stdlib registers the "pgx" database/sql driver.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Runner applies migrations against a database.
type Runner struct {
	engine *migrate.Migrate
}

// New builds a Runner from a database/sql handle and a directory of migration
// files.
func New(db *sql.DB, dir string) (*Runner, error) {
	driver, err := pgx.WithInstance(db, &pgx.Config{})
	if err != nil {
		return nil, fmt.Errorf("migrations: build driver: %w", err)
	}
	engine, err := migrate.NewWithDatabaseInstance("file://"+dir, "pgx", driver)
	if err != nil {
		return nil, fmt.Errorf("migrations: build engine: %w", err)
	}
	return &Runner{engine: engine}, nil
}

// Up applies every pending migration.
func (r *Runner) Up() error {
	if err := r.engine.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrations: up: %w", err)
	}
	return nil
}

// Down rolls back the most recent migration. steps of 0 or less roll back one.
func (r *Runner) Down(steps int) error {
	if steps <= 0 {
		steps = 1
	}
	if err := r.engine.Steps(-steps); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrations: down: %w", err)
	}
	return nil
}

// Version reports the applied schema version and whether the database is dirty.
func (r *Runner) Version() (version uint, dirty bool, err error) {
	version, dirty, err = r.engine.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("migrations: version: %w", err)
	}
	return version, dirty, nil
}

// Close releases the migration engine.
func (r *Runner) Close() error {
	sourceErr, databaseErr := r.engine.Close()
	return errors.Join(sourceErr, databaseErr)
}

// Files returns the migration file names present in dir, for diagnostics.
func Files(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("migrations: read %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}
