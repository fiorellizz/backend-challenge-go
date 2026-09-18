package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// OutboxService publishes committed events. Several instances run it at
// once: each claims its own batch with SKIP LOCKED, so they never publish
// the same row concurrently, and a claim abandoned by a crash is released
// by the database when the transaction aborts.
type OutboxService struct {
	uow         UnitOfWork
	publisher   EventPublisher
	now         Clock
	batchSize   int
	maxAttempts int
	log         *slog.Logger
}

// NewOutboxService wires the service to its ports.
func NewOutboxService(uow UnitOfWork, publisher EventPublisher, now Clock, batchSize, maxAttempts int, log *slog.Logger) (*OutboxService, error) {
	if batchSize <= 0 || maxAttempts <= 0 {
		return nil, fmt.Errorf("outbox batch size and max attempts must be positive")
	}
	return &OutboxService{
		uow: uow, publisher: publisher, now: now, batchSize: batchSize, maxAttempts: maxAttempts,
		log: log.With("component", "outbox"),
	}, nil
}

// PublishOutcome counts what one batch did.
type PublishOutcome struct {
	Claimed   int
	Published int
	Failed    int
}

const (
	outboxBaseBackoff = time.Second
	outboxMaxBackoff  = 5 * time.Minute
)

// PublishBatch claims due events, publishes each and records the result,
// all inside one transaction. A publish failure marks that event for a
// later attempt with exponential backoff and does not affect the others.
//
// The commit comes after the publish, so a crash in between republishes
// the event with the same EventID: consumers deduplicate on it. An event is
// never dropped; past maxAttempts the backoff stays at its cap and the
// failure is logged at error level for operators.
func (s *OutboxService) PublishBatch(ctx context.Context) (PublishOutcome, error) {
	var out PublishOutcome
	err := s.uow.WithinTx(ctx, func(ctx context.Context, r Repositories) error {
		now := s.now()
		records, err := r.Outbox.ClaimPending(ctx, s.batchSize, now)
		if err != nil {
			return err
		}
		out.Claimed = len(records)
		for _, rec := range records {
			if err := s.publisher.Publish(ctx, rec); err != nil {
				out.Failed++
				if err := s.recordFailure(ctx, r, rec, err, now); err != nil {
					return err
				}
				continue
			}
			if err := r.Outbox.MarkPublished(ctx, rec.Envelope.EventID, s.now()); err != nil {
				return err
			}
			out.Published++
		}
		return nil
	})
	return out, err
}

func (s *OutboxService) recordFailure(ctx context.Context, r Repositories, rec OutboxRecord, cause error, now time.Time) error {
	attempts := rec.Attempts + 1
	next := now.Add(backoff(attempts))
	level := slog.LevelWarn
	if attempts >= s.maxAttempts {
		level = slog.LevelError
	}
	s.log.Log(ctx, level, "event publish failed",
		"eventId", rec.Envelope.EventID, "eventType", rec.Envelope.EventType,
		"attempts", attempts, "nextAttemptAt", next, "error", cause.Error())
	return r.Outbox.MarkFailed(ctx, rec.Envelope.EventID, attempts, next, cause.Error())
}

// backoff grows as base * 2^(attempts-1), capped.
func backoff(attempts int) time.Duration {
	d := outboxBaseBackoff
	for i := 1; i < attempts && d < outboxMaxBackoff; i++ {
		d *= 2
	}
	if d > outboxMaxBackoff {
		d = outboxMaxBackoff
	}
	return d
}
