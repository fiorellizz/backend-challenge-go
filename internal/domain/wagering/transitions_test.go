package wagering_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
)

func TestMarkProcessedStoresResult(t *testing.T) {
	tx := external(t, wagering.Bet, "25.00")
	later := now.Add(time.Second)

	if err := tx.MarkProcessed(brl(t, "975.00"), 2, later); err != nil {
		t.Fatal(err)
	}
	balance, version, ok := tx.Result()
	if !ok || balance.Amount() != "975.00" || version != 2 || tx.Status() != wagering.Processed {
		t.Fatalf("result = %s, %d, %v; status %s", balance, version, ok, tx.Status())
	}
	if s := tx.Snapshot(); !s.ProcessedAt.Equal(later) || !s.UpdatedAt.Equal(later) {
		t.Fatalf("timestamps not updated: %+v", s)
	}
}

func TestMarkProcessedValidatesResult(t *testing.T) {
	usd, _ := money.New(1, "USD")
	if err := external(t, wagering.Bet, "1.00").MarkProcessed(usd, 1, now); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("other currency: %v", err)
	}
	if err := external(t, wagering.Bet, "1.00").MarkProcessed(money.Money{}, 1, now); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("zero money: %v", err)
	}
	if err := external(t, wagering.Bet, "1.00").MarkProcessed(brl(t, "1.00"), 0, now); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("version zero: %v", err)
	}
}

func TestTerminalStatesRefuseEveryTransition(t *testing.T) {
	terminal := map[string]func(*wagering.WagerTransaction) error{
		"processed": func(tx *wagering.WagerTransaction) error { return tx.MarkProcessed(brl(t, "1.00"), 1, now) },
		"rejected":  func(tx *wagering.WagerTransaction) error { return tx.MarkRejected(wagering.InsufficientBalance, now) },
		"failed":    func(tx *wagering.WagerTransaction) error { return tx.MarkFailed(wagering.PermanentFailure, now) },
	}
	for name, finish := range terminal {
		t.Run(name, func(t *testing.T) {
			tx := external(t, wagering.Refund, "1.00")
			if err := finish(tx); err != nil {
				t.Fatal(err)
			}
			if !tx.Status().IsTerminal() {
				t.Fatalf("status %s is not terminal", tx.Status())
			}
			before := tx.Snapshot()

			attempts := []error{
				tx.MarkProcessed(brl(t, "1.00"), 1, now),
				tx.MarkRejected(wagering.InsufficientBalance, now),
				tx.MarkFailed(wagering.PermanentFailure, now),
				tx.AwaitReference(policy, now),
				tx.RetryReference(policy, now),
				tx.ResolveReference(refID),
			}
			for i, err := range attempts {
				if !errors.Is(err, wagering.ErrTerminalState) {
					t.Errorf("attempt %d: error = %v, want ErrTerminalState", i, err)
				}
				if !errors.Is(err, errs.ErrConflict) {
					t.Errorf("attempt %d: terminal violation must classify as conflict", i)
				}
			}
			after := tx.Snapshot()
			if after.Status != before.Status || after.FailureCode != before.FailureCode || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("terminal transaction changed: %+v -> %+v", before, after)
			}
		})
	}
}

func TestRejectionRequiresCode(t *testing.T) {
	tx := external(t, wagering.Bet, "1.00")
	if err := tx.MarkRejected("", now); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("error = %v", err)
	}
	if tx.Status() != wagering.Pending {
		t.Fatalf("status changed on invalid rejection")
	}
}

func TestAwaitReferenceOnlyFromPendingReversal(t *testing.T) {
	bet := external(t, wagering.Bet, "1.00")
	if err := bet.AwaitReference(policy, now); !errors.Is(err, wagering.ErrInvalidTransition) {
		t.Errorf("bet: %v", err)
	}

	refund := external(t, wagering.Refund, "1.00")
	if err := refund.AwaitReference(policy, now); err != nil {
		t.Fatal(err)
	}
	s := refund.Snapshot()
	if s.Status != wagering.PendingReference || s.ReferenceAttempts != 0 {
		t.Fatalf("unexpected state: %+v", s)
	}
	if !s.NextReferenceAttemptAt.Equal(now.Add(2*time.Second)) || !s.ReferenceDeadlineAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("schedule: next %s, deadline %s", s.NextReferenceAttemptAt, s.ReferenceDeadlineAt)
	}
	if err := refund.AwaitReference(policy, now); !errors.Is(err, wagering.ErrInvalidTransition) {
		t.Errorf("await twice: %v", err)
	}
	if err := external(t, wagering.Refund, "1.00").AwaitReference(wagering.ReferencePolicy{}, now); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("empty policy: %v", err)
	}
}

func TestRetryReferenceBacksOffExponentiallyUntilExhausted(t *testing.T) {
	tx := external(t, wagering.Rollback, "1.00")
	if err := tx.AwaitReference(policy, now); err != nil {
		t.Fatal(err)
	}

	// attempt 1 → next in 4s, attempt 2 → 8s, attempt 3 → 16s, attempt 4 → exhausted (MaxAttempts=4).
	want := []time.Duration{4 * time.Second, 8 * time.Second, 16 * time.Second}
	for i, d := range want {
		at := now.Add(time.Duration(i+1) * time.Second)
		if err := tx.RetryReference(policy, at); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		if got := tx.NextReferenceAttemptAt().Sub(at); got != d {
			t.Fatalf("attempt %d: backoff %s, want %s", i+1, got, d)
		}
		if tx.ReferenceAttempts() != i+1 {
			t.Fatalf("attempts = %d", tx.ReferenceAttempts())
		}
	}
	err := tx.RetryReference(policy, now.Add(10*time.Second))
	if !errors.Is(err, wagering.ErrReferenceExpired) {
		t.Fatalf("4th attempt: %v, want ErrReferenceExpired", err)
	}
	if tx.Status() != wagering.PendingReference {
		t.Fatalf("expiry must leave the decision to the caller, got %s", tx.Status())
	}
	if err := tx.MarkRejected(wagering.ReferenceNotFound, now); err != nil {
		t.Fatal(err)
	}
}

func TestRetryReferenceExpiresOnTTL(t *testing.T) {
	tx := external(t, wagering.Refund, "1.00")
	if err := tx.AwaitReference(policy, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.RetryReference(policy, now.Add(time.Minute)); !errors.Is(err, wagering.ErrReferenceExpired) {
		t.Fatalf("at deadline: %v", err)
	}
}

func TestRetryReferenceBackoffIsCappedByTTL(t *testing.T) {
	long := wagering.ReferencePolicy{BaseBackoff: 40 * time.Second, MaxAttempts: 10, TTL: time.Minute}
	tx := external(t, wagering.Refund, "1.00")
	if err := tx.AwaitReference(long, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.RetryReference(long, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := tx.NextReferenceAttemptAt().Sub(now.Add(time.Second)); got != time.Minute {
		t.Fatalf("backoff %s, want capped at TTL", got)
	}
}

func TestRetryReferenceOnlyFromPendingReference(t *testing.T) {
	if err := external(t, wagering.Refund, "1.00").RetryReference(policy, now); !errors.Is(err, wagering.ErrInvalidTransition) {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveReference(t *testing.T) {
	tx := external(t, wagering.Refund, "1.00")
	if err := tx.ResolveReference("bad"); err == nil {
		t.Errorf("bad id accepted")
	}
	if err := tx.ResolveReference(refID); err != nil || tx.ReferenceID() != refID {
		t.Fatalf("resolve: %v, ref %s", err, tx.ReferenceID())
	}
	if err := external(t, wagering.Bet, "1.00").ResolveReference(refID); !errors.Is(err, wagering.ErrInvalidTransition) {
		t.Errorf("bet: %v", err)
	}
	if err := external(t, wagering.Win, "1.00").ResolveReference(refID); err != nil {
		t.Errorf("win may resolve an optional reference: %v", err)
	}
}

func TestPendingReferenceCanStillBeProcessedOrRejected(t *testing.T) {
	tx := external(t, wagering.Refund, "1.00")
	_ = tx.AwaitReference(policy, now)
	if err := tx.MarkProcessed(brl(t, "1.00"), 3, now); err != nil {
		t.Fatalf("PENDING_REFERENCE -> PROCESSED: %v", err)
	}

	tx = external(t, wagering.Refund, "1.00")
	_ = tx.AwaitReference(policy, now)
	if err := tx.MarkRejected(wagering.ReferenceNotFound, now); err != nil || tx.FailureCode() != wagering.ReferenceNotFound {
		t.Fatalf("PENDING_REFERENCE -> REJECTED: %v", err)
	}
}
