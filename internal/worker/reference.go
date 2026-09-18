// Package worker holds the background loops: the pending-reference
// resolver and the outbox publisher. Each exposes Run(ctx), which returns
// only when ctx is cancelled, and RunOnce, which the tests drive directly.
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// ReferenceResolver retries reversals parked as PENDING_REFERENCE.
type ReferenceResolver struct {
	wagering *usecase.WageringService
	interval time.Duration
	log      *slog.Logger
}

// NewReferenceResolver builds the worker. interval is how long it sleeps
// when nothing is due or after an infrastructure error.
func NewReferenceResolver(wagering *usecase.WageringService, interval time.Duration, log *slog.Logger) *ReferenceResolver {
	return &ReferenceResolver{wagering: wagering, interval: interval, log: log.With("component", "reference-resolver")}
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
		found, err := r.wagering.ResolveNextPendingReference(ctx)
		if err != nil {
			return handled, err
		}
		if !found {
			return handled, nil
		}
		handled++
	}
	return handled, ctx.Err()
}
