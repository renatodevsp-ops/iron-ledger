// Package config loads and validates the whole application configuration from
// the environment. Everything the process needs to start is resolved here, and
// an invalid configuration is a startup failure rather than a surprise on the
// first request.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the validated configuration of an iron-ledger process.
type Config struct {
	Environment string
	ServiceName string
	InstanceID  string
	LogLevel    string

	HTTP     HTTP
	Database Database
	SQS      SQS
	Auth     Auth
	Wagering Wagering
	Outbox   Outbox
	Shutdown Shutdown
}

// HTTP configures the public API listener.
type HTTP struct {
	Address        string
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	IdleTimeout    time.Duration
	ShutdownBudget time.Duration
	MaxBodyBytes   int64
	TrustedProxies []string
}

// Database configures the PostgreSQL connection pool.
type Database struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
	// Migrations enables running pending migrations on boot. It is on for the
	// API and off for workers/tests that migrate explicitly.
	Migrations bool
}

// SQS configures the message broker.
type SQS struct {
	Endpoint       string
	Region         string
	AccessKey      string
	SecretKey      string
	QueueURL       string
	DLQURL         string
	EventsQueueURL string
	// VisibilityTimeout must exceed the worst-case processing time; a message
	// still in flight when it expires becomes visible again and is redelivered,
	// which the inbox absorbs.
	VisibilityTimeout time.Duration
	WaitTime          time.Duration
	MaxMessages       int32
	MaxAttempts       int
}

// Auth configures the external OpenID Connect identity provider.
type Auth struct {
	Enabled bool
	Issuer  string
	// Audience is the expected `aud`/azp of an access token.
	Audience string
	// InternalClientID is the service account allowed to perform wallet
	// operations.
	InternalClientID string
	// InternalRole is the role/claim value that marks the internal caller.
	InternalRole string
	// ClientProviders maps an OAuth client_id to the provider identity it is
	// allowed to act as, for IdPs that do not project a provider claim into
	// the token. Format: "provider-a=provider-a,provider-b=provider-b".
	ClientProviders map[string]string
	// ProviderClaim is the token claim carrying the caller's provider id.
	ProviderClaim string
	// RoleClaim is the token claim carrying the caller's roles.
	RoleClaim string
	// Leeway tolerates small clock differences between this process and the
	// IdP.
	Leeway time.Duration
}

// Wagering tunes the wager operation state machine.
type Wagering struct {
	// MaxReferenceAttempts bounds how long a transaction waits for a missing
	// or not-yet-processed reference before it is rejected.
	MaxReferenceAttempts int
	// ReferenceBackoffBase is the first retry delay for pending references.
	ReferenceBackoffBase time.Duration
	// ReferenceBackoffMax caps the exponential backoff.
	ReferenceBackoffMax time.Duration
	// PendingResumeInterval is how often the resumer looks for transactions
	// left PENDING by a crash.
	PendingResumeInterval time.Duration
	// MaxConflictRetries bounds the optimistic-concurrency retry budget of a
	// single business operation.
	MaxConflictRetries int
}

// Outbox tunes the transactional outbox publisher.
type Outbox struct {
	PollInterval     time.Duration
	BatchSize        int
	VisibilityWindow time.Duration
	MaxAttempts      int
	BackoffBase      time.Duration
	BackoffMax       time.Duration
	QueueURL         string
}

// Shutdown bounds process termination.
type Shutdown struct {
	SignalTimeout time.Duration
}

// Load reads the configuration from the environment, applying defaults that
// make `docker compose up` work with no .env file at all.
func Load() (Config, error) {
	instance, err := instanceID()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Environment: env("IRONLEDGER_ENV", "local"),
		ServiceName: env("IRONLEDGER_SERVICE_NAME", "iron-ledger"),
		InstanceID:  instance,
		LogLevel:    env("LOG_LEVEL", "info"),
	}

	cfg.HTTP = HTTP{
		Address:        env("HTTP_ADDRESS", ":8080"),
		ReadTimeout:    envDuration("HTTP_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:   envDuration("HTTP_WRITE_TIMEOUT", 15*time.Second),
		IdleTimeout:    envDuration("HTTP_IDLE_TIMEOUT", 60*time.Second),
		ShutdownBudget: envDuration("HTTP_SHUTDOWN_BUDGET", 20*time.Second),
		MaxBodyBytes:   int64(envInt("HTTP_MAX_BODY_BYTES", 1<<20)),
		TrustedProxies: envList("HTTP_TRUSTED_PROXIES", nil),
	}

	cfg.Database = Database{
		DSN:             env("DATABASE_URL", "postgres://iron:iron@localhost:5432/ironledger?sslmode=disable"),
		MaxConns:        int32(envInt("DATABASE_MAX_CONNS", 10)),
		MinConns:        int32(envInt("DATABASE_MIN_CONNS", 1)),
		MaxConnLifetime: envDuration("DATABASE_MAX_CONN_LIFETIME", 30*time.Minute),
		MaxConnIdleTime: envDuration("DATABASE_MAX_CONN_IDLE_TIME", 5*time.Minute),
		ConnectTimeout:  envDuration("DATABASE_CONNECT_TIMEOUT", 5*time.Second),
		Migrations:      envBool("DATABASE_MIGRATE_ON_BOOT", true),
	}

	cfg.SQS = SQS{
		Endpoint:          env("SQS_ENDPOINT", "http://localhost:4566"),
		Region:            env("AWS_REGION", "us-east-1"),
		AccessKey:         env("AWS_ACCESS_KEY_ID", "test"),
		SecretKey:         env("AWS_SECRET_ACCESS_KEY", "test"),
		QueueURL:          env("SQS_WAGER_QUEUE_URL", ""),
		DLQURL:            env("SQS_WAGER_DLQ_URL", ""),
		EventsQueueURL:    env("SQS_EVENTS_QUEUE_URL", ""),
		VisibilityTimeout: envDuration("SQS_VISIBILITY_TIMEOUT", 30*time.Second),
		WaitTime:          envDuration("SQS_WAIT_TIME", 5*time.Second),
		MaxMessages:       int32(envInt("SQS_MAX_MESSAGES", 10)),
		MaxAttempts:       envInt("SQS_MAX_ATTEMPTS", 5),
	}

	cfg.Auth = Auth{
		Enabled:          envBool("AUTH_ENABLED", true),
		Issuer:           env("OIDC_ISSUER_URL", "http://localhost:8081/realms/ironledger"),
		Audience:         env("OIDC_AUDIENCE", "iron-ledger-api"),
		InternalClientID: env("AUTH_INTERNAL_CLIENT_ID", "iron-ledger-internal"),
		InternalRole:     env("AUTH_INTERNAL_ROLE", "internal"),
		ClientProviders: envMap("AUTH_CLIENT_PROVIDERS", map[string]string{
			"provider-a": "provider-a",
			"provider-b": "provider-b",
		}),
		ProviderClaim: env("AUTH_PROVIDER_CLAIM", "providerId"),
		RoleClaim:     env("AUTH_ROLE_CLAIM", "roles"),
		Leeway:        envDuration("AUTH_LEEWAY", 30*time.Second),
	}

	cfg.Wagering = Wagering{
		MaxReferenceAttempts:  envInt("WAGERING_MAX_REFERENCE_ATTEMPTS", 8),
		ReferenceBackoffBase:  envDuration("WAGERING_REFERENCE_BACKOFF_BASE", 2*time.Second),
		ReferenceBackoffMax:   envDuration("WAGERING_REFERENCE_BACKOFF_MAX", 2*time.Minute),
		PendingResumeInterval: envDuration("WAGERING_PENDING_RESUME_INTERVAL", 5*time.Second),
		MaxConflictRetries:    envInt("WAGERING_MAX_CONFLICT_RETRIES", 8),
	}

	cfg.Outbox = Outbox{
		PollInterval:     envDuration("OUTBOX_POLL_INTERVAL", 250*time.Millisecond),
		BatchSize:        envInt("OUTBOX_BATCH_SIZE", 50),
		VisibilityWindow: envDuration("OUTBOX_VISIBILITY_WINDOW", 30*time.Second),
		MaxAttempts:      envInt("OUTBOX_MAX_ATTEMPTS", 10),
		BackoffBase:      envDuration("OUTBOX_BACKOFF_BASE", time.Second),
		BackoffMax:       envDuration("OUTBOX_BACKOFF_MAX", time.Minute),
		QueueURL:         env("SQS_EVENTS_QUEUE_URL", ""),
	}

	cfg.Shutdown = Shutdown{
		SignalTimeout: envDuration("SHUTDOWN_SIGNAL_TIMEOUT", 30*time.Second),
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects a configuration the process could not honour.
func (c Config) Validate() error {
	if c.Database.DSN == "" {
		return fmt.Errorf("config: DATABASE_URL is required")
	}
	if c.Database.MaxConns < 1 {
		return fmt.Errorf("config: DATABASE_MAX_CONNS must be >= 1, got %d", c.Database.MaxConns)
	}
	if c.Auth.Enabled {
		if c.Auth.Issuer == "" {
			return fmt.Errorf("config: OIDC_ISSUER_URL is required when AUTH_ENABLED")
		}
		if c.Auth.Audience == "" {
			return fmt.Errorf("config: OIDC_AUDIENCE is required when AUTH_ENABLED")
		}
		if c.Auth.InternalClientID == "" {
			return fmt.Errorf("config: AUTH_INTERNAL_CLIENT_ID is required when AUTH_ENABLED")
		}
	}
	if c.Wagering.MaxReferenceAttempts < 1 {
		return fmt.Errorf("config: WAGERING_MAX_REFERENCE_ATTEMPTS must be >= 1, got %d", c.Wagering.MaxReferenceAttempts)
	}
	if c.Outbox.BatchSize < 1 {
		return fmt.Errorf("config: OUTBOX_BATCH_SIZE must be >= 1, got %d", c.Outbox.BatchSize)
	}
	if c.HTTP.MaxBodyBytes < 1 {
		return fmt.Errorf("config: HTTP_MAX_BODY_BYTES must be >= 1, got %d", c.HTTP.MaxBodyBytes)
	}
	return nil
}

// ProviderForClient resolves the provider a service account is allowed to act
// as, or "" when the mapping does not cover it.
func (a Auth) ProviderForClient(clientID string) string {
	return a.ClientProviders[clientID]
}

func instanceID() (string, error) {
	if v := os.Getenv("INSTANCE_ID"); v != "" {
		return v, nil
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return host + "-" + strconv.Itoa(os.Getpid()), nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func envList(key string, fallback []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envMap parses a "k=v,k=v" list.
func envMap(key string, fallback map[string]string) map[string]string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	out := make(map[string]string, len(fallback))
	for k, val := range fallback {
		out[k] = val
	}
	for _, pair := range strings.Split(v, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, val, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	return out
}
