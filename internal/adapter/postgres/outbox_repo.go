package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/event"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// OutboxRepository implements usecase.OutboxRepository.
type OutboxRepository struct {
	q querier
}

const (
	// The payload is serialized at insert time: it is the immutable
	// snapshot that will be published, however many attempts it takes.
	insertOutbox = `
		INSERT INTO outbox_events
			(event_id, aggregate_type, aggregate_id, event_type, event_version,
			 correlation_id, causation_id, payload, occurred_at, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

	// Publishers on different instances claim disjoint sets thanks to SKIP
	// LOCKED; a claim abandoned by a crash is released with its transaction.
	claimOutbox = `
		SELECT event_id, aggregate_type, aggregate_id, event_type, event_version,
		       correlation_id, causation_id, payload, occurred_at, attempts
		  FROM outbox_events
		 WHERE published_at IS NULL AND next_attempt_at <= $1
		 ORDER BY next_attempt_at, event_id
		 LIMIT $2
		 FOR UPDATE SKIP LOCKED`

	markOutboxPublished = `
		UPDATE outbox_events SET published_at = $2, attempts = attempts + 1, last_error = NULL
		 WHERE event_id = ANY($1) AND published_at IS NULL`

	markOutboxFailed = `
		UPDATE outbox_events SET attempts = $2, next_attempt_at = $3, last_error = $4
		 WHERE event_id = $1 AND published_at IS NULL`

	oldestPendingOutbox = `
		SELECT COALESCE(MIN(occurred_at), $1) FROM outbox_events WHERE published_at IS NULL`
)

func (r *OutboxRepository) Insert(ctx context.Context, env event.Envelope) error {
	payload, err := json.Marshal(env.Data)
	if err != nil {
		return fmt.Errorf("serialize event %s: %w", env.EventID, err)
	}
	_, err = r.q.Exec(ctx, insertOutbox,
		env.EventID, env.AggregateType, env.AggregateID, env.EventType, env.Version,
		env.CorrelationID, nullStr(env.CausationID), payload, env.OccurredAt.UTC(), env.OccurredAt.UTC())
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", mapError(err))
	}
	return nil
}

func (r *OutboxRepository) ClaimPending(ctx context.Context, limit int, now time.Time) ([]usecase.OutboxRecord, error) {
	rows, err := r.q.Query(ctx, claimOutbox, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", mapError(err))
	}
	defer rows.Close()

	var out []usecase.OutboxRecord
	for rows.Next() {
		var rec usecase.OutboxRecord
		var causation *string
		if err := rows.Scan(
			&rec.Envelope.EventID, &rec.Envelope.AggregateType, &rec.Envelope.AggregateID,
			&rec.Envelope.EventType, &rec.Envelope.Version, &rec.Envelope.CorrelationID, &causation,
			&rec.Data, &rec.Envelope.OccurredAt, &rec.Attempts,
		); err != nil {
			return nil, fmt.Errorf("scan outbox event: %w", mapError(err))
		}
		rec.Envelope.CausationID = deref(causation)
		rec.Envelope.OccurredAt = rec.Envelope.OccurredAt.UTC()
		rec.Envelope.Data = rec.Data
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim outbox: %w", mapError(err))
	}
	return out, nil
}

func (r *OutboxRepository) MarkPublished(ctx context.Context, eventIDs []string, now time.Time) error {
	if len(eventIDs) == 0 {
		return nil
	}
	tag, err := r.q.Exec(ctx, markOutboxPublished, eventIDs, now.UTC())
	if err != nil {
		return fmt.Errorf("mark published: %w", mapError(err))
	}
	if tag.RowsAffected() != int64(len(eventIDs)) {
		return fmt.Errorf("mark published: %d of %d rows updated: %w", tag.RowsAffected(), len(eventIDs), errs.ErrNotFound)
	}
	return nil
}

func (r *OutboxRepository) MarkFailed(ctx context.Context, eventID string, attempts int, nextAttemptAt time.Time, lastError string) error {
	if _, err := r.q.Exec(ctx, markOutboxFailed, eventID, attempts, nextAttemptAt.UTC(), lastError); err != nil {
		return fmt.Errorf("mark failed: %w", mapError(err))
	}
	return nil
}

func (r *OutboxRepository) OldestPendingAge(ctx context.Context, now time.Time) (time.Duration, error) {
	var oldest time.Time
	if err := r.q.QueryRow(ctx, oldestPendingOutbox, now.UTC()).Scan(&oldest); err != nil {
		return 0, fmt.Errorf("outbox lag: %w", mapError(err))
	}
	return now.Sub(oldest), nil
}
