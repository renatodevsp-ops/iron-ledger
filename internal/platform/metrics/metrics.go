// Package metrics owns every Prometheus collector the service exposes.
//
// The registry is process-wide and built once at composition time, so metric
// names are stable and a duplicate registration is a startup failure rather
// than a silent second collector.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the full set of collectors exposed on /metrics.
type Metrics struct {
	registry *prometheus.Registry

	// OperationResults counts business outcomes by kind and terminal status.
	OperationResults *prometheus.CounterVec
	// OperationLatency observes the wall time of a business operation.
	OperationLatency *prometheus.HistogramVec
	// IdempotentReplays counts requests recognised as a replay of an already
	// processed operation.
	IdempotentReplays *prometheus.CounterVec
	// IdempotencyConflicts counts reused keys carrying a different payload.
	IdempotencyConflicts *prometheus.CounterVec
	// ConcurrencyConflicts counts lost-update races and the retries they cost.
	ConcurrencyConflicts *prometheus.CounterVec
	// InboxDuplicates counts messages recognised as already handled.
	InboxDuplicates *prometheus.CounterVec
	// InboxCompleted counts messages whose durable processing committed.
	InboxCompleted *prometheus.CounterVec
	// InboxFailures counts messages that failed and were left for redelivery.
	InboxFailures *prometheus.CounterVec
	// DLQMessages counts messages that exhausted their retry budget.
	DLQMessages *prometheus.CounterVec
	// OutboxPublished counts events handed to the broker.
	OutboxPublished *prometheus.CounterVec
	// OutboxRetries counts publication attempts that failed and were rescheduled.
	OutboxRetries *prometheus.CounterVec
	// OutboxLag observes how long a committed event waited before publication.
	OutboxLag prometheus.Histogram
	// ReconciliationDivergences counts reconciliations that found a mismatch.
	ReconciliationDivergences *prometheus.CounterVec
	// DependencyUp reports reachability of PostgreSQL and SQS.
	DependencyUp *prometheus.GaugeVec
	// WorkerRunning reports whether each long-running worker is active.
	WorkerRunning *prometheus.GaugeVec
}

// New builds the collectors and registers them on a fresh registry.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
	}

	m.OperationResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ironledger_operation_results_total",
		Help: "Business operations by kind and resulting status.",
	}, []string{"kind", "status"})

	m.OperationLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ironledger_operation_latency_seconds",
		Help:    "End-to-end latency of a business operation, from arrival to durable commit.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 12),
	}, []string{"kind", "source"})

	m.IdempotentReplays = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ironledger_idempotent_replays_total",
		Help: "Requests recognised as a replay of an already processed operation.",
	}, []string{"kind", "source"})

	m.IdempotencyConflicts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ironledger_idempotency_conflicts_total",
		Help: "Reused idempotency keys carrying a different business payload.",
	}, []string{"kind", "source"})

	m.ConcurrencyConflicts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ironledger_concurrency_conflicts_total",
		Help: "Lost-update races on a wallet stream, and the retries spent resolving them.",
	}, []string{"outcome"})

	m.InboxDuplicates = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ironledger_inbox_duplicates_total",
		Help: "Inbound messages already durably handled, recognised through the inbox.",
	}, []string{"consumer"})

	m.InboxCompleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ironledger_inbox_completed_total",
		Help: "Inbound messages whose durable processing committed.",
	}, []string{"consumer", "outcome"})

	m.InboxFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ironledger_inbox_failures_total",
		Help: "Inbound messages that failed and were left for redelivery.",
	}, []string{"consumer", "kind"})

	m.DLQMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ironledger_dlq_messages_total",
		Help: "Messages that exhausted their retry budget and were sent to the dead-letter queue.",
	}, []string{"queue"})

	m.OutboxPublished = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ironledger_outbox_published_total",
		Help: "Integration events handed to the broker by the outbox publisher.",
	}, []string{"eventType"})

	m.OutboxRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ironledger_outbox_retries_total",
		Help: "Outbox publication attempts that failed and were rescheduled with backoff.",
	}, []string{"eventType"})

	m.OutboxLag = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "ironledger_outbox_lag_seconds",
		Help:    "Delay between a committed event and its publication.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
	})

	m.ReconciliationDivergences = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ironledger_reconciliation_divergences_total",
		Help: "Wallet reconciliations where the stored balance differs from the ledger sum.",
	}, []string{"walletId"})

	m.DependencyUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ironledger_dependency_up",
		Help: "Reachability of an external dependency (1 up, 0 down).",
	}, []string{"dependency"})

	m.WorkerRunning = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ironledger_worker_running",
		Help: "Whether a long-running worker is currently active (1 running, 0 stopped).",
	}, []string{"worker"})

	m.registry.MustRegister(
		m.OperationResults,
		m.OperationLatency,
		m.IdempotentReplays,
		m.IdempotencyConflicts,
		m.ConcurrencyConflicts,
		m.InboxDuplicates,
		m.InboxCompleted,
		m.InboxFailures,
		m.DLQMessages,
		m.OutboxPublished,
		m.OutboxRetries,
		m.OutboxLag,
		m.ReconciliationDivergences,
		m.DependencyUp,
		m.WorkerRunning,
	)

	return m
}

// Registry exposes the collector registry to the HTTP handler.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }
