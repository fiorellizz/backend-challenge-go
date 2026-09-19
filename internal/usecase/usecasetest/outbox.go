package usecasetest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/event"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

type outboxRepo struct{ s *Store }

func (r *outboxRepo) Insert(_ context.Context, env event.Envelope) error {
	if err := r.s.fail("Outbox.Insert"); err != nil {
		return err
	}
	for _, row := range r.s.Outbox {
		if row.Record.Envelope.EventID == env.EventID {
			return fmt.Errorf("%w: outbox_events_pkey", errs.ErrConflict)
		}
	}
	data, err := json.Marshal(env.Data)
	if err != nil {
		return err
	}
	r.s.Outbox = append(r.s.Outbox, OutboxRow{
		Record:      usecase.OutboxRecord{Envelope: env, Data: data},
		NextAttempt: env.OccurredAt,
	})
	return nil
}

func (r *outboxRepo) ClaimPending(_ context.Context, limit int, now time.Time) ([]usecase.OutboxRecord, error) {
	if err := r.s.fail("Outbox.ClaimPending"); err != nil {
		return nil, err
	}
	var out []usecase.OutboxRecord
	for _, row := range r.s.Outbox {
		if !row.Published && !row.NextAttempt.After(now) && len(out) < limit {
			out = append(out, row.Record)
		}
	}
	return out, nil
}

func (r *outboxRepo) MarkPublished(_ context.Context, eventIDs []string, _ time.Time) error {
	if err := r.s.fail("Outbox.MarkPublished"); err != nil {
		return err
	}
	for _, eventID := range eventIDs {
		found := false
		for i := range r.s.Outbox {
			if r.s.Outbox[i].Record.Envelope.EventID == eventID && !r.s.Outbox[i].Published {
				r.s.Outbox[i].Published = true
				r.s.Outbox[i].Record.Attempts++
				found = true
			}
		}
		if !found {
			return fmt.Errorf("event %s: %w", eventID, errs.ErrNotFound)
		}
	}
	return nil
}

func (r *outboxRepo) MarkFailed(_ context.Context, eventID string, attempts int, nextAttemptAt time.Time, lastError string) error {
	for i := range r.s.Outbox {
		if r.s.Outbox[i].Record.Envelope.EventID == eventID {
			r.s.Outbox[i].Record.Attempts = attempts
			r.s.Outbox[i].NextAttempt = nextAttemptAt
			r.s.Outbox[i].LastError = lastError
			return nil
		}
	}
	return nil
}

func (r *outboxRepo) OldestPendingAge(_ context.Context, now time.Time) (time.Duration, error) {
	var oldest time.Time
	for _, row := range r.s.Outbox {
		if !row.Published && (oldest.IsZero() || row.Record.Envelope.OccurredAt.Before(oldest)) {
			oldest = row.Record.Envelope.OccurredAt
		}
	}
	if oldest.IsZero() {
		return 0, nil
	}
	return now.Sub(oldest), nil
}
