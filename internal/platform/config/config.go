// Package config loads and validates the process configuration from the
// environment. Every setting has a default, and every setting that has no safe
// default is required, so the process refuses to start rather than starting
// misconfigured and failing requests later.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ironledger/ironledger/internal/platform/httpapi"
	"github.com/ironledger/ironledger/internal/platform/keycloak"
)

// Config is the whole process configuration.
type Config struct {
	// Environment names the deployment stage, used in logs and in the readiness
	// payload. It never changes behaviour beyond log verbosity.
	Environment string
	LogLevel    string

	HTTP httpapi.Config
	DB   DBConfig
	Auth AuthConfig

	// RunMigrations controls whether this process applies migrations at start.
	// It defaults to false: a rolling deploy must not have every replica racing to
	// run DDL. The migration task is a separate, deliberate step.
	RunMigrations bool

	// ShutdownTimeout bounds the graceful drain.
	ShutdownTimeout time.Duration
}

// DBConfig is the PostgreSQL connection configuration.
type DBConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
	// StatementTimeout is applied per connection. It is the backstop for a
	// statement that would otherwise hold a row lock indefinitely.
	StatementTimeout time.Duration
}

// AuthConfig configures token verification.
type AuthConfig struct {
	RealmURL       string
	JWKSURL        string
	Audience       string
	AllowedClients []string
	WriteScope     string
	ReadScope      string
}

// Load reads the configuration from the environment. It returns every problem it
// finds at once, so a misconfigured deployment can be fixed in one pass instead
// of one variable per restart.
func Load() (Config, error) {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	cfg := Config{
		Environment: env("IRONLEDGER_ENV", "development"),
		LogLevel:    env("IRONLEDGER_LOG_LEVEL", "info"),
		HTTP: httpapi.Config{
			ListenAddr:        env("IRONLEDGER_HTTP_ADDR", ":8080"),
			ReadTimeout:       envDuration("IRONLEDGER_HTTP_READ_TIMEOUT", 15*time.Second),
			ReadHeaderTimeout: envDuration("IRONLEDGER_HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
			WriteTimeout:      envDuration("IRONLEDGER_HTTP_WRITE_TIMEOUT", 15*time.Second),
			IdleTimeout:       envDuration("IRONLEDGER_HTTP_IDLE_TIMEOUT", 60*time.Second),
			WriteScope:        env("IRONLEDGER_WRITE_SCOPE", "wallet:write"),
			ReadScope:         env("IRONLEDGER_READ_SCOPE", "wallet:read"),
		},
		DB: DBConfig{
			URL:              os.Getenv("IRONLEDGER_DATABASE_URL"),
			MaxConns:         int32(envInt("IRONLEDGER_DB_MAX_CONNS", 20)),
			MinConns:         int32(envInt("IRONLEDGER_DB_MIN_CONNS", 2)),
			MaxConnLifetime:  envDuration("IRONLEDGER_DB_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime:  envDuration("IRONLEDGER_DB_CONN_IDLE", 30*time.Minute),
			ConnectTimeout:   envDuration("IRONLEDGER_DB_CONNECT_TIMEOUT", 5*time.Second),
			StatementTimeout: envDuration("IRONLEDGER_DB_STATEMENT_TIMEOUT", 10*time.Second),
		},
		Auth: AuthConfig{
			RealmURL:       os.Getenv("IRONLEDGER_KEYCLOAK_REALM_URL"),
			JWKSURL:        os.Getenv("IRONLEDGER_KEYCLOAK_JWKS_URL"),
			Audience:       os.Getenv("IRONLEDGER_KEYCLOAK_AUDIENCE"),
			AllowedClients: envList("IRONLEDGER_KEYCLOAK_ALLOWED_CLIENTS"),
			// The transport and the verifier must agree on the scope names, so the
			// two config blocks are filled from the same environment variables.
			WriteScope: env("IRONLEDGER_WRITE_SCOPE", "wallet:write"),
			ReadScope:  env("IRONLEDGER_READ_SCOPE", "wallet:read"),
		},
		RunMigrations:   envBool("IRONLEDGER_RUN_MIGRATIONS", false),
		ShutdownTimeout: envDuration("IRONLEDGER_SHUTDOWN_TIMEOUT", 25*time.Second),
	}

	if cfg.DB.URL == "" {
		fail("IRONLEDGER_DATABASE_URL is required")
	} else if !strings.HasPrefix(cfg.DB.URL, "postgres://") &&
		!strings.HasPrefix(cfg.DB.URL, "postgresql://") {
		// A common mistake is pointing this at the Keycloak or SQS URL. Saying so
		// plainly is cheaper than a driver error deep in the pool constructor.
		fail("IRONLEDGER_DATABASE_URL must be a postgres:// or postgresql:// URL, got %q", redactURL(cfg.DB.URL))
	}
	if cfg.DB.MinConns > cfg.DB.MaxConns {
		fail("IRONLEDGER_DB_MIN_CONNS (%d) must not exceed IRONLEDGER_DB_MAX_CONNS (%d)",
			cfg.DB.MinConns, cfg.DB.MaxConns)
	}

	// A verifier that cannot reject a token is worse than no verifier, so the
	// auth settings are validated with the same rules as the verifier itself.
	if _, err := keycloak.New(keycloak.Config{
		RealmURL:       cfg.Auth.RealmURL,
		JWKSURL:        cfg.Auth.JWKSURL,
		Audience:       cfg.Auth.Audience,
		AllowedClients: cfg.Auth.AllowedClients,
	}); err != nil {
		fail("auth: %v", err)
	}

	if len(problems) > 0 {
		return cfg, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// redactURL hides the password in a DSN so a misconfiguration message can be
// pasted into a ticket.
func redactURL(raw string) string {
	at := strings.LastIndex(raw, "@")
	slashes := strings.Index(raw, "//")
	if at < 0 || slashes < 0 || at < slashes {
		return raw
	}
	credentials := raw[slashes+2 : at]
	if colon := strings.Index(credentials, ":"); colon >= 0 {
		credentials = credentials[:colon] + ":***"
	}
	return raw[:slashes+2] + credentials + raw[at:]
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envList(key string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func envInt(key string, fallback int) int {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		// An unparseable value must not silently become the default: a typo in a
		// pool size is a configuration bug, and starting anyway hides it.
		return fallback
	}
	return v
}

func envBool(key string, fallback bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return v
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return v
}
