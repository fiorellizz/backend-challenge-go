package usecase_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase/usecasetest"
)

// fakePublisher records every publish and can fail selected events.
type fakePublisher struct {
	published []string
	failing   map[string]bool
}

func (p *fakePublisher) Publish(_ context.Context, recs []usecase.OutboxRecord) []error {
	results := make([]error, len(recs))
	for i, rec := range recs {
		if p.failing[rec.Envelope.EventID] {
			results[i] = errors.New("broker unavailable")
			continue
		}
		p.published = append(p.published, rec.Envelope.EventID)
	}
	return results
}

func outboxFixture(t *testing.T) (*fixture, *fakePublisher, *usecase.OutboxService) {
	t.Helper()
	f := newFixture(t, "100.00") // opening produced two events
	pub := &fakePublisher{failing: map[string]bool{}}
	svc, err := usecase.NewOutboxService(f.store, pub, func() time.Time { return f.now }, 10, 3, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return f, pub, svc
}

func TestPublishBatchPublishesAndMarksEvents(t *testing.T) {
	f, pub, svc := outboxFixture(t)
	f.process(t, f.input(t, "BET", "bet-1", "10.00")) // two more events

	out, err := svc.PublishBatch(context.Background())
	if err != nil || out.Claimed != 4 || out.Published != 4 || out.Failed != 0 {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	if len(pub.published) != 4 || len(f.store.PendingOutbox()) != 0 {
		t.Fatalf("published %d, pending %d", len(pub.published), len(f.store.PendingOutbox()))
	}
	for _, row := range f.store.Outbox {
		if !row.Published || row.Record.Attempts != 1 {
			t.Fatalf("row not marked: %+v", row)
		}
	}
	out, _ = svc.PublishBatch(context.Background())
	if out.Claimed != 0 {
		t.Fatalf("published events claimed again: %+v", out)
	}
}

func TestPublishFailureSchedulesRetryWithBackoff(t *testing.T) {
	f, pub, svc := outboxFixture(t)
	failing := f.store.PendingOutbox()[0].Envelope.EventID
	pub.failing[failing] = true

	out, err := svc.PublishBatch(context.Background())
	if err != nil || out.Claimed != 2 || out.Published != 1 || out.Failed != 1 {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	var row usecasetest.OutboxRow
	for _, r := range f.store.Outbox {
		if r.Record.Envelope.EventID == failing {
			row = r
		}
	}
	if row.Published || row.Record.Attempts != 1 || !row.NextAttempt.Equal(f.now.Add(time.Second)) || row.LastError == "" {
		t.Fatalf("failed row: %+v", row)
	}

	// Not due yet: nothing is claimed.
	if out, _ := svc.PublishBatch(context.Background()); out.Claimed != 0 {
		t.Fatalf("claimed before backoff: %+v", out)
	}
	// Second failure doubles the backoff.
	f.now = f.now.Add(time.Second)
	if out, _ := svc.PublishBatch(context.Background()); out.Failed != 1 {
		t.Fatalf("second attempt: %+v", out)
	}
	for _, r := range f.store.Outbox {
		if r.Record.Envelope.EventID == failing && (r.Record.Attempts != 2 || !r.NextAttempt.Equal(f.now.Add(2*time.Second))) {
			t.Fatalf("after second failure: %+v", r)
		}
	}
	// Broker back: the event goes out with its original id.
	delete(pub.failing, failing)
	f.now = f.now.Add(2 * time.Second)
	if out, _ := svc.PublishBatch(context.Background()); out.Published != 1 {
		t.Fatalf("recovery: %+v", out)
	}
	if pub.published[len(pub.published)-1] != failing || len(f.store.PendingOutbox()) != 0 {
		t.Fatalf("recovered event not published: %v", pub.published)
	}
}

func TestCrashBetweenPublishAndCommitRepublishesSameEventID(t *testing.T) {
	f, pub, svc := outboxFixture(t)
	// The database refuses the mark: everything the batch did rolls back,
	// but the broker already received the message.
	f.store.FailOn, f.store.FailErr = "Outbox.MarkPublished", errs.ErrTransient
	if _, err := svc.PublishBatch(context.Background()); !errors.Is(err, errs.ErrTransient) {
		t.Fatalf("error = %v", err)
	}
	if len(pub.published) != 2 || len(f.store.PendingOutbox()) != 2 {
		t.Fatalf("published %d, pending %d", len(pub.published), len(f.store.PendingOutbox()))
	}

	f.store.FailOn = ""
	if out, err := svc.PublishBatch(context.Background()); err != nil || out.Published != 2 {
		t.Fatalf("second run: %+v %v", out, err)
	}
	// Both events were delivered twice with the same ids: consumers
	// deduplicate on eventId. That is the at-least-once contract.
	seen := map[string]int{}
	for _, id := range pub.published {
		seen[id]++
	}
	if len(seen) != 2 {
		t.Fatalf("republication changed event ids: %v", pub.published)
	}
	for id, n := range seen {
		if n != 2 {
			t.Fatalf("event %s published %d times, want 2", id, n)
		}
	}
}

func TestOutboxServiceValidatesSettings(t *testing.T) {
	if _, err := usecase.NewOutboxService(nil, nil, nil, 0, 1, slog.Default()); err == nil {
		t.Fatal("zero batch accepted")
	}
}
