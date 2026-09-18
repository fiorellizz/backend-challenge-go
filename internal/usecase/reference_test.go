package usecase_test

import (
	"context"
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/event"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

func (f *fixture) reversal(t *testing.T, kind, external, reference, amount string) usecase.ProcessInput {
	t.Helper()
	in := f.input(t, kind, external, amount)
	in.ReferenceExternalTransactionID = reference
	return in
}

func (f *fixture) process(t *testing.T, in usecase.ProcessInput) *wagering.WagerTransaction {
	t.Helper()
	res, err := f.wagering.Process(context.Background(), in)
	if err != nil {
		t.Fatalf("process %s %s: %v", in.Kind, in.ExternalTransactionID, err)
	}
	return res.Transaction
}

func (f *fixture) resolveOnce(t *testing.T) bool {
	t.Helper()
	found, err := f.wagering.ResolveNextPendingReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestRefundCreditsBetBack(t *testing.T) {
	f := newFixture(t, "100.00")
	f.process(t, f.input(t, "BET", "bet-1", "30.00"))

	refund := f.process(t, f.reversal(t, "REFUND", "refund-1", "bet-1", "30.00"))
	balance, version, _ := refund.Result()
	if refund.Status() != wagering.Processed || balance.Amount() != "100.00" || version != 3 {
		t.Fatalf("refund: %s %s v%d", refund.Status(), balance, version)
	}
	if refund.ReferenceID() == "" {
		t.Fatalf("resolved reference not stored")
	}
	if last := f.store.Ledger[len(f.store.Ledger)-1].Entry; last.Direction() != wallet.Credit || last.Amount().Amount() != "30.00" {
		t.Fatalf("refund entry: %+v", last)
	}
}

func TestRollbackReversesEachKind(t *testing.T) {
	f := newFixture(t, "100.00")
	f.process(t, f.input(t, "BET", "bet-1", "30.00"))                   // 70
	f.process(t, f.input(t, "WIN", "win-1", "50.00"))                   // 120
	f.process(t, f.input(t, "BET", "bet-2", "10.00"))                   // 110
	f.process(t, f.reversal(t, "REFUND", "refund-2", "bet-2", "10.00")) // 120

	cases := []struct {
		external, reference, want string
		direction                 wallet.Direction
	}{
		{"rb-bet", "bet-1", "150.00", wallet.Credit},
		{"rb-win", "win-1", "100.00", wallet.Debit},
		{"rb-refund", "refund-2", "90.00", wallet.Debit},
	}
	for _, tc := range cases {
		amount := map[string]string{"bet-1": "30.00", "win-1": "50.00", "refund-2": "10.00"}[tc.reference]
		tx := f.process(t, f.reversal(t, "ROLLBACK", tc.external, tc.reference, amount))
		if tx.Status() != wagering.Processed {
			t.Fatalf("%s: %s %s", tc.external, tx.Status(), tx.FailureCode())
		}
		if got, _ := f.balance(t); got != tc.want {
			t.Fatalf("%s: balance %s, want %s", tc.external, got, tc.want)
		}
		if last := f.store.Ledger[len(f.store.Ledger)-1].Entry; last.Direction() != tc.direction {
			t.Fatalf("%s: direction %s", tc.external, last.Direction())
		}
	}
}

func TestOnlyOneSuccessfulReversalPerReference(t *testing.T) {
	f := newFixture(t, "100.00")
	f.process(t, f.input(t, "BET", "bet-1", "30.00"))
	f.process(t, f.reversal(t, "REFUND", "refund-1", "bet-1", "30.00"))

	rollback := f.process(t, f.reversal(t, "ROLLBACK", "rb-1", "bet-1", "30.00"))
	if rollback.Status() != wagering.Rejected || rollback.FailureCode() != wagering.ReferenceAlreadyReversed {
		t.Fatalf("second reversal: %s %s", rollback.Status(), rollback.FailureCode())
	}
	second := f.process(t, f.reversal(t, "REFUND", "refund-1b", "bet-1", "30.00"))
	if second.FailureCode() != wagering.ReferenceAlreadyReversed {
		t.Fatalf("second refund: %s", second.FailureCode())
	}
	if got, _ := f.balance(t); got != "100.00" {
		t.Fatalf("balance %s", got)
	}
}

func TestRejectedReversalDoesNotBlockALaterOne(t *testing.T) {
	f := newFixture(t, "100.00")
	f.process(t, f.input(t, "BET", "bet-1", "30.00"))
	bad := f.process(t, f.reversal(t, "REFUND", "refund-wrong", "bet-1", "31.00"))
	if bad.FailureCode() != wagering.ReferenceMismatch {
		t.Fatalf("amount mismatch: %s", bad.FailureCode())
	}
	good := f.process(t, f.reversal(t, "REFUND", "refund-ok", "bet-1", "30.00"))
	if good.Status() != wagering.Processed {
		t.Fatalf("valid refund after a rejected one: %s %s", good.Status(), good.FailureCode())
	}
}

func TestReversalDebitBeyondBalanceHasItsOwnCode(t *testing.T) {
	f := newFixture(t, "0.00")
	f.process(t, f.input(t, "WIN", "win-1", "50.00"))
	f.process(t, f.input(t, "BET", "bet-1", "40.00")) // balance 10

	rb := f.process(t, f.reversal(t, "ROLLBACK", "rb-win", "win-1", "50.00"))
	if rb.Status() != wagering.Rejected || rb.FailureCode() != wagering.ReversalInsufficientBalance {
		t.Fatalf("rollback of win beyond balance: %s %s", rb.Status(), rb.FailureCode())
	}
	if got, _ := f.balance(t); got != "10.00" {
		t.Fatalf("balance %s", got)
	}
}

func TestReferenceNotProcessedIsRejected(t *testing.T) {
	f := newFixture(t, "10.00")
	f.process(t, f.input(t, "BET", "bet-big", "50.00")) // rejected: insufficient balance
	rb := f.process(t, f.reversal(t, "ROLLBACK", "rb-1", "bet-big", "50.00"))
	if rb.FailureCode() != wagering.ReferenceNotProcessed {
		t.Fatalf("code %s", rb.FailureCode())
	}
}

func TestReversalBeforeReferenceIsParkedThenResolved(t *testing.T) {
	f := newFixture(t, "100.00")

	refund := f.process(t, f.reversal(t, "REFUND", "refund-1", "bet-late", "30.00"))
	if refund.Status() != wagering.PendingReference {
		t.Fatalf("status %s", refund.Status())
	}
	if refund.NextReferenceAttemptAt() != f.now.Add(time.Second) {
		t.Fatalf("first retry at %s", refund.NextReferenceAttemptAt())
	}
	if f.eventTypes()[event.TypeWagerTransactionPendingReference] != 1 {
		t.Fatalf("missing pending event: %v", f.eventTypes())
	}
	if got, _ := f.balance(t); got != "100.00" {
		t.Fatalf("parked reversal moved money: %s", got)
	}

	// Replay while parked returns the parked state, no reprocessing.
	replay, err := f.wagering.Process(context.Background(), f.reversal(t, "REFUND", "refund-1", "bet-late", "30.00"))
	if err != nil || !replay.IdempotentReplay || replay.Transaction.Status() != wagering.PendingReference {
		t.Fatalf("replay: %v %+v", err, replay)
	}

	// Not due yet: the worker finds nothing.
	if f.resolveOnce(t) {
		t.Fatalf("resolved before the backoff elapsed")
	}

	// Due, still absent: attempts grow and the next attempt is pushed.
	f.now = f.now.Add(time.Second)
	if !f.resolveOnce(t) {
		t.Fatalf("due reference not claimed")
	}
	stored := f.store.Transactions[refund.ID()]
	if stored.Status != wagering.PendingReference || stored.ReferenceAttempts != 1 || !stored.NextReferenceAttemptAt.Equal(f.now.Add(2*time.Second)) {
		t.Fatalf("after first retry: %+v", stored)
	}

	// The bet arrives; on the next due attempt the refund settles.
	f.process(t, f.input(t, "BET", "bet-late", "30.00"))
	f.now = f.now.Add(2 * time.Second)
	if !f.resolveOnce(t) {
		t.Fatalf("not claimed after reference arrived")
	}
	stored = f.store.Transactions[refund.ID()]
	if stored.Status != wagering.Processed || stored.BalanceAfter.Amount() != "100.00" || stored.WalletVersionAfter != 3 {
		t.Fatalf("after resolution: %+v", stored)
	}
	if got, v := f.balance(t); got != "100.00" || v != 3 {
		t.Fatalf("wallet %s v%d", got, v)
	}
	if f.resolveOnce(t) {
		t.Fatalf("nothing should remain pending")
	}
}

func TestParkedReversalExpiresAsRejected(t *testing.T) {
	f := newFixture(t, "100.00")
	rb := f.process(t, f.reversal(t, "ROLLBACK", "rb-1", "bet-never", "30.00"))

	// policy: base 1s, max 3 attempts → attempts at +1s, +2s, +4s; the 3rd exhausts.
	for _, step := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		f.now = f.now.Add(step)
		if !f.resolveOnce(t) {
			t.Fatalf("not claimed at +%s", step)
		}
	}
	stored := f.store.Transactions[rb.ID()]
	if stored.Status != wagering.Rejected || stored.FailureCode != wagering.ReferenceNotFound || stored.ReferenceAttempts != 3 {
		t.Fatalf("after expiry: %+v", stored)
	}
	if f.eventTypes()[event.TypeWagerTransactionRejected] != 1 {
		t.Fatalf("missing rejected event: %v", f.eventTypes())
	}

	// A replay after expiry sees the definitive rejection.
	replay, err := f.wagering.Process(context.Background(), f.reversal(t, "ROLLBACK", "rb-1", "bet-never", "30.00"))
	if err != nil || !replay.IdempotentReplay || replay.Transaction.FailureCode() != wagering.ReferenceNotFound {
		t.Fatalf("replay: %v %+v", err, replay)
	}
}

func TestParkedReversalWithBadReferenceIsRejectedOnResolution(t *testing.T) {
	f := newFixture(t, "100.00")
	rb := f.process(t, f.reversal(t, "ROLLBACK", "rb-1", "bet-late", "30.00"))

	f.process(t, f.input(t, "BET", "bet-late", "25.00")) // amount differs from the rollback
	f.now = f.now.Add(time.Second)
	f.resolveOnce(t)
	stored := f.store.Transactions[rb.ID()]
	if stored.Status != wagering.Rejected || stored.FailureCode != wagering.ReferenceMismatch {
		t.Fatalf("after resolution: %+v", stored)
	}
}

func TestWinMayNameAnOptionalReference(t *testing.T) {
	f := newFixture(t, "100.00")
	win := f.process(t, f.reversal(t, "WIN", "win-1", "bet-missing", "5.00"))
	if win.Status() != wagering.Processed || win.ReferenceID() != "" {
		t.Fatalf("win with absent reference: %s ref=%q", win.Status(), win.ReferenceID())
	}
	bet := f.process(t, f.input(t, "BET", "bet-1", "5.00"))
	win2 := f.process(t, f.reversal(t, "WIN", "win-2", "bet-1", "5.00"))
	if win2.Status() != wagering.Processed || win2.ReferenceID() != bet.ID() {
		t.Fatalf("win with present reference: %s ref=%q", win2.Status(), win2.ReferenceID())
	}
}
