package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// OutboxPublisher drains the outbox in batches. When a batch was full it
// immediately claims the next one; otherwise it waits for the poll
// interval, so an idle instance costs one query per interval.
type OutboxPublisher struct {
	outbox   *usecase.OutboxService
	interval time.Duration
	log      *slog.Logger
}

// NewOutboxPublisher builds the worker.
func NewOutboxPublisher(outbox *usecase.OutboxService, interval time.Duration, log *slog.Logger) *OutboxPublisher {
	return &OutboxPublisher{outbox: outbox, interval: interval, log: log.With("component", "outbox-publisher")}
}

// Run publishes until ctx is cancelled. A batch in progress completes (or
// rolls back) before Run returns; the transaction is the unit of work.
func (p *OutboxPublisher) Run(ctx context.Context) {
	p.log.Info("outbox publisher started", "interval", p.interval.String())
	defer p.log.Info("outbox publisher stopped")
	for {
		outcome, err := p.outbox.PublishBatch(ctx)
		if err != nil && ctx.Err() == nil {
			p.log.Warn("outbox batch failed; will retry", "error", err.Error())
		}
		if err == nil && outcome.Claimed > 0 && outcome.Failed == 0 {
			// More work is likely waiting; do not sleep.
			if ctx.Err() == nil {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.interval):
		}
	}
}
