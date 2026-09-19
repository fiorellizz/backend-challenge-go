package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/platform/metrics"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// OutboxPublisher drains the outbox in batches. When a batch was full it
// immediately claims the next one; otherwise it waits for the poll
// interval, so an idle instance costs one query per interval.
type OutboxPublisher struct {
	outbox   *usecase.OutboxService
	reads    usecase.Repositories
	interval time.Duration
	metrics  *metrics.Metrics
	log      *slog.Logger
}

// NewOutboxPublisher builds the worker.
func NewOutboxPublisher(outbox *usecase.OutboxService, reads usecase.Repositories, interval time.Duration, m *metrics.Metrics, log *slog.Logger) *OutboxPublisher {
	return &OutboxPublisher{outbox: outbox, reads: reads, interval: interval, metrics: m, log: log.With("component", "outbox-publisher")}
}

// Run publishes until ctx is cancelled. A batch in progress completes (or
// rolls back) before Run returns; the transaction is the unit of work.
func (p *OutboxPublisher) Run(ctx context.Context) {
	p.log.Info("outbox publisher started", "interval", p.interval.String())
	defer p.log.Info("outbox publisher stopped")
	for {
		outcome, err := p.publishBatch(ctx)
		if err != nil && ctx.Err() == nil {
			p.log.Warn("outbox batch failed; will retry", "error", err.Error())
		}
		p.metrics.OutboxPublishedTotal.Add(float64(outcome.Published))
		p.metrics.OutboxFailedTotal.Add(float64(outcome.Failed))
		if err == nil && outcome.Claimed > 0 && outcome.Failed == 0 {
			// More work is likely waiting; do not sleep.
			if ctx.Err() == nil {
				continue
			}
		}
		p.observeLag(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.interval):
		}
	}
}

// publishBatch bounds one batch so a frozen dependency cannot hang the
// loop; the claim is rolled back on timeout and retried later.
func (p *OutboxPublisher) publishBatch(ctx context.Context) (usecase.PublishOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, iterationTimeout)
	defer cancel()
	return p.outbox.PublishBatch(ctx)
}

// observeLag refreshes the gauge whenever the loop goes idle, so the
// metric reflects events other instances may be failing to publish too.
func (p *OutboxPublisher) observeLag(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	lag, err := p.outbox.Lag(ctx, p.reads)
	if err != nil {
		return
	}
	p.metrics.OutboxLagSeconds.Set(lag.Seconds())
}
