package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/ironledger/iron-ledger/internal/app/usecase"
	"github.com/ironledger/iron-ledger/internal/platform/config"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
	"github.com/ironledger/iron-ledger/internal/sharedkernel/xerr"
)

// ReferenceResolver retries the reversals that arrived before their reference.
//
// It is the second half of the PENDING_REFERENCE contract: the first half is
// that the wait is durable — the attempt count and the next attempt time live
// in the database, not in this process — and this worker is what makes the wait
// end. It runs in every instance, claims rows by their due time, and settles
// whatever has become resolvable. A reference that never arrives exhausts its
// budget and the reversal is rejected with a stable code.
type ReferenceResolver struct {
	wagering *usecase.WageringUseCase
	cfg      config.Wagering
	logger   *slog.Logger
	metrics  *metrics.Metrics
}

// NewReferenceResolver builds the resolver.
func NewReferenceResolver(
	wagering *usecase.WageringUseCase,
	cfg config.Wagering,
	logger *slog.Logger,
	m *metrics.Metrics,
) *ReferenceResolver {
	return &ReferenceResolver{wagering: wagering, cfg: cfg, logger: logger, metrics: m}
}

// Run resolves pending references until ctx is cancelled.
func (r *ReferenceResolver) Run(ctx context.Context) error {
	r.setRunning(true)
	defer r.setRunning(false)

	r.logger.Info("pending reference resolver started",
		"interval", r.cfg.PendingResumeInterval.String(),
		"maxAttempts", r.cfg.MaxReferenceAttempts,
	)

	ticker := time.NewTicker(r.cfg.PendingResumeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

func (r *ReferenceResolver) sweep(ctx context.Context) {
	due, err := r.wagering.DuePending(ctx, batchSize)
	if err != nil {
		if ctx.Err() == nil {
			r.logger.Warn("could not list pending references", "error", err.Error())
		}
		return
	}

	for _, entity := range due {
		if ctx.Err() != nil {
			return
		}
		result, err := r.wagering.Resume(ctx, entity)
		if err != nil {
			if xerr.IsTerminal(err) {
				// The reversal was rejected and the decision is recorded; the
				// next sweep will not see it.
				continue
			}
			r.logger.Warn("pending reference could not be resolved",
				"transactionId", entity.ID.String(),
				"attempts", entity.PendingAttempts,
				"error", err.Error())
			continue
		}
		if r.metrics != nil {
			r.metrics.OperationResults.WithLabelValues(entity.Kind, result.Status).Inc()
		}
		if result.Status == "PROCESSED" {
			r.logger.Info("pending reference resolved",
				"transactionId", entity.ID.String(),
				"attempts", entity.PendingAttempts)
		}
	}
}

func (r *ReferenceResolver) setRunning(running bool) {
	if r.metrics == nil {
		return
	}
	if running {
		r.metrics.WorkerRunning.WithLabelValues("reference_resolver").Set(1)
	} else {
		r.metrics.WorkerRunning.WithLabelValues("reference_resolver").Set(0)
	}
}

// batchSize bounds how many pending references one sweep takes on. Several
// instances sweep the same table, so a small batch keeps them from piling onto
// the same rows.
const batchSize = 25
