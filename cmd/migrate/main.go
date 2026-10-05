// Command migrate applies or rolls back the database schema.
//
// Usage:
//
//	migrate up          apply every pending migration
//	migrate down [n]    roll back the last n migrations (default 1)
//	migrate version     print the applied version
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ironledger/iron-ledger/internal/platform/migrations"
)

func main() {
	dsn := flag.String("dsn", envOr("DATABASE_URL", "postgres://iron:iron@localhost:5432/ironledger?sslmode=disable"), "database connection string")
	dir := flag.String("dir", envOr("MIGRATIONS_DIR", "migrations"), "directory holding the migration files")
	flag.Parse()

	command := flag.Arg(0)
	if command == "" {
		command = "up"
	}

	db, err := sql.Open("pgx", *dsn)
	if err != nil {
		fatal("open database: %v", err)
	}
	defer db.Close()

	runner, err := migrations.New(db, *dir)
	if err != nil {
		fatal("%v", err)
	}
	defer runner.Close()

	switch command {
	case "up":
		if err := runner.Up(); err != nil {
			fatal("%v", err)
		}
	case "down":
		steps := 1
		if flag.NArg() > 1 {
			if _, err := fmt.Sscanf(flag.Arg(1), "%d", &steps); err != nil {
				fatal("down expects a step count: %v", err)
			}
		}
		if err := runner.Down(steps); err != nil {
			fatal("%v", err)
		}
	case "version":
		version, dirty, err := runner.Version()
		if err != nil {
			fatal("%v", err)
		}
		fmt.Printf("version=%d dirty=%t\n", version, dirty)
	default:
		fatal("unknown command %q: expected up, down or version", command)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "migrate: "+format+"\n", args...)
	os.Exit(1)
}
