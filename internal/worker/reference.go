// Package worker holds the background loops: the pending-reference
// resolver and the outbox publisher. Each exposes Run(ctx), which returns
// only when ctx is cancelled, and RunOnce, which the tests drive directly.
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/platform/metrics"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// iterationTimeout bounds one unit of worker work (a batch, a claim) so a
// dependency that stops answering cannot hang a loop indefinitely.
const iterationTimeout = 30 * time.Second

// ReferenceResolver retries reversals parked as PENDING_REFERENCE.
type ReferenceResolver struct {
	wagering *usecase.WageringService
	interval time.Duration
	metrics  *metrics.Metrics
	log      *slog.Logger
}

// NewReferenceResolver builds the worker. interval is how long it sleeps
// when nothing is due or after an infrastructure error.
func NewReferenceResolver(wagering *usecase.WageringService, interval time.Duration, m *metrics.Metrics, log *slog.Logger) *ReferenceResolver {
	return &ReferenceResolver{wagering: wagering, interval: interval, metrics: m, log: log.With("component", "reference-resolver")}
}

// Run drains due references, then sleeps for the interval, until ctx is
// cancelled. A claim in progress finishes before Run returns because the
// use case holds the transaction, not the loop.
func (r *ReferenceResolver) Run(ctx context.Context) {
	r.log.Info("reference resolver started", "interval", r.interval.String())
	defer r.log.Info("reference resolver stopped")
	for {
		if _, err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("reference resolution failed; will retry", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.interval):
		}
	}
}

// RunOnce resolves every reference that is currently due and returns how
// many it handled. It stops early on the first error or when ctx ends.
func (r *ReferenceResolver) RunOnce(ctx context.Context) (int, error) {
	handled := 0
	for ctx.Err() == nil {
		outcome, found, err := r.resolveOne(ctx)
		if err != nil {
			return handled, err
		}
		if !found {
			return handled, nil
		}
		handled++
		r.metrics.ReferenceResolutionsTotal.WithLabelValues(string(outcome)).Inc()
		r.log.Info("pending reference handled", "outcome", string(outcome))
	}
	return handled, ctx.Err()
}

func (r *ReferenceResolver) resolveOne(ctx context.Context) (usecase.ReferenceOutcome, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, iterationTimeout)
	defer cancel()
	return r.wagering.ResolveNextPendingReference(ctx)
}
