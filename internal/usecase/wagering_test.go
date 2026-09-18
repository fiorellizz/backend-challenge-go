package usecase_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/event"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase/usecasetest"
)

var testPolicy = wagering.ReferencePolicy{BaseBackoff: time.Second, MaxAttempts: 3, TTL: time.Minute}

type fixture struct {
	store    *usecasetest.Store
	wallets  *usecase.WalletService
	wagering *usecase.WageringService
	walletID id.WalletID
	now      time.Time // advanced by tests to move through backoff windows
}

func newFixture(t *testing.T, balance string) *fixture {
	t.Helper()
	f := &fixture{store: usecasetest.NewStore(), now: fixedNow}
	clock := func() time.Time { return f.now }
	f.wallets = usecase.NewWalletService(f.store, f.store.Repos(), clock, quietLog())
	svc, err := usecase.NewWageringService(f.store, f.store.Repos(), clock, testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	f.wagering = svc
	w, err := f.wallets.Open(context.Background(), usecase.OpenWalletInput{PlayerID: playerID, InitialBalance: brl(t, balance)})
	if err != nil {
		t.Fatal(err)
	}
	f.walletID = w.ID()
	return f
}

func (f *fixture) input(t *testing.T, kind, external, amount string) usecase.ProcessInput {
	t.Helper()
	return usecase.ProcessInput{
		IdempotencyKey: "provider-a:" + external, ProviderID: "provider-a", ExternalTransactionID: external,
		PlayerID: playerID.String(), WalletID: f.walletID.String(), RoundID: "round-1", GameID: "fortune-chimp",
		Kind: kind, Money: brl(t, amount), CorrelationID: "corr-" + external,
	}
}

func (f *fixture) balance(t *testing.T) (string, int64) {
	t.Helper()
	w, err := f.wallets.Get(context.Background(), f.walletID)
	if err != nil {
		t.Fatal(err)
	}
	return w.Balance().Amount(), w.Version()
}

func (f *fixture) eventTypes() map[string]int {
	out := map[string]int{}
	for _, rec := range f.store.PendingOutbox() {
		out[rec.Envelope.EventType]++
	}
	return out
}

func TestBetDebitsWalletAndRecordsEverything(t *testing.T) {
	f := newFixture(t, "1000.00")
	res, err := f.wagering.Process(context.Background(), f.input(t, "BET", "tx-1", "25.00"))
	if err != nil {
		t.Fatal(err)
	}
	tx := res.Transaction
	balance, version, ok := tx.Result()
	if res.IdempotentReplay || tx.Status() != wagering.Processed || !ok || balance.Amount() != "975.00" || version != 2 {
		t.Fatalf("result: replay=%v status=%s balance=%s v%d", res.IdempotentReplay, tx.Status(), balance, version)
	}
	if got, v := f.balance(t); got != "975.00" || v != 2 {
		t.Fatalf("wallet: %s v%d", got, v)
	}
	if len(f.store.Ledger) != 2 || f.store.Ledger[1].Entry.Direction() != wallet.Debit || f.store.Ledger[1].Entry.TransactionID() != tx.ID() {
		t.Fatalf("ledger: %+v", f.store.Ledger)
	}
	types := f.eventTypes()
	if types[event.TypeWagerTransactionProcessed] != 2 || types[event.TypeWalletBalanceChanged] != 2 {
		t.Fatalf("events after opening + bet: %v", types)
	}
	for _, rec := range f.store.PendingOutbox() {
		if rec.Envelope.CausationID == tx.ID().String() && rec.Envelope.CorrelationID != "corr-tx-1" {
			t.Fatalf("correlation not propagated: %+v", rec.Envelope)
		}
	}
}

func TestWinCreditsAndLossMovesNothing(t *testing.T) {
	f := newFixture(t, "100.00")
	if _, err := f.wagering.Process(context.Background(), f.input(t, "WIN", "tx-w", "50.00")); err != nil {
		t.Fatal(err)
	}
	if got, v := f.balance(t); got != "150.00" || v != 2 {
		t.Fatalf("after win: %s v%d", got, v)
	}

	res, err := f.wagering.Process(context.Background(), f.input(t, "LOSS", "tx-l", "0.00"))
	if err != nil {
		t.Fatal(err)
	}
	balance, version, _ := res.Transaction.Result()
	if res.Transaction.Status() != wagering.Processed || balance.Amount() != "150.00" || version != 2 {
		t.Fatalf("loss result: %s %s v%d", res.Transaction.Status(), balance, version)
	}
	if got, v := f.balance(t); got != "150.00" || v != 2 {
		t.Fatalf("loss changed the wallet: %s v%d", got, v)
	}
	if len(f.store.Ledger) != 2 {
		t.Fatalf("loss created a ledger entry: %d entries", len(f.store.Ledger))
	}
	types := f.eventTypes()
	if types[event.TypeWagerTransactionProcessed] != 3 || types[event.TypeWalletBalanceChanged] != 2 {
		t.Fatalf("loss must emit Processed without BalanceChanged: %v", types)
	}
}

func TestInsufficientBalanceIsPersistedRejection(t *testing.T) {
	f := newFixture(t, "100.00")
	res, err := f.wagering.Process(context.Background(), f.input(t, "BET", "tx-big", "100.01"))
	if err != nil {
		t.Fatalf("rejection must not be an error: %v", err)
	}
	tx := res.Transaction
	if tx.Status() != wagering.Rejected || tx.FailureCode() != wagering.InsufficientBalance {
		t.Fatalf("status %s code %s", tx.Status(), tx.FailureCode())
	}
	if _, _, ok := tx.Result(); ok {
		t.Fatalf("rejected transaction must not carry a result")
	}
	if got, v := f.balance(t); got != "100.00" || v != 1 {
		t.Fatalf("wallet touched by rejection: %s v%d", got, v)
	}
	if len(f.store.Ledger) != 1 {
		t.Fatalf("rejection created a ledger entry")
	}
	if f.eventTypes()[event.TypeWagerTransactionRejected] != 1 {
		t.Fatalf("missing rejected event: %v", f.eventTypes())
	}
	stored := f.store.Transactions[tx.ID()]
	if stored.Status != wagering.Rejected || stored.FailureCode != wagering.InsufficientBalance {
		t.Fatalf("rejection not persisted: %+v", stored)
	}
}

func TestReplayReturnsOriginalResultEvenAfterOtherMovements(t *testing.T) {
	f := newFixture(t, "1000.00")
	first, err := f.wagering.Process(context.Background(), f.input(t, "BET", "tx-1", "25.00"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.wagering.Process(context.Background(), f.input(t, "BET", "tx-2", "100.00")); err != nil {
		t.Fatal(err)
	}

	in := f.input(t, "BET", "tx-1", "25")    // equivalent amount form
	in.CorrelationID = "another-correlation" // transport metadata differs
	replay, err := f.wagering.Process(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	balance, version, _ := replay.Transaction.Result()
	if !replay.IdempotentReplay || replay.Transaction.ID() != first.Transaction.ID() || balance.Amount() != "975.00" || version != 2 {
		t.Fatalf("replay: %+v balance %s v%d", replay, balance, version)
	}
	if got, v := f.balance(t); got != "875.00" || v != 3 {
		t.Fatalf("replay must not move money: %s v%d", got, v)
	}
	if len(f.store.Ledger) != 3 || len(f.store.Transactions) != 3 {
		t.Fatalf("replay persisted something: %d ledger, %d tx", len(f.store.Ledger), len(f.store.Transactions))
	}
}

func TestRejectedOutcomeReplaysAsRejected(t *testing.T) {
	f := newFixture(t, "10.00")
	if _, err := f.wagering.Process(context.Background(), f.input(t, "BET", "tx-1", "50.00")); err != nil {
		t.Fatal(err)
	}
	replay, err := f.wagering.Process(context.Background(), f.input(t, "BET", "tx-1", "50.00"))
	if err != nil || !replay.IdempotentReplay || replay.Transaction.Status() != wagering.Rejected {
		t.Fatalf("replay: %v %+v", err, replay)
	}
}

func TestSameKeyDifferentPayloadConflicts(t *testing.T) {
	f := newFixture(t, "1000.00")
	if _, err := f.wagering.Process(context.Background(), f.input(t, "BET", "tx-1", "25.00")); err != nil {
		t.Fatal(err)
	}
	in := f.input(t, "BET", "tx-1", "26.00")
	_, err := f.wagering.Process(context.Background(), in)
	if !errors.Is(err, usecase.ErrPayloadMismatch) || !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("error = %v", err)
	}
	if got, _ := f.balance(t); got != "975.00" {
		t.Fatalf("conflict moved money: %s", got)
	}
}

func TestSameOperationUnderAnotherKeyConflicts(t *testing.T) {
	f := newFixture(t, "1000.00")
	if _, err := f.wagering.Process(context.Background(), f.input(t, "BET", "tx-1", "25.00")); err != nil {
		t.Fatal(err)
	}
	in := f.input(t, "BET", "tx-1", "25.00")
	in.IdempotencyKey = "provider-a:some-other-key"
	_, err := f.wagering.Process(context.Background(), in)
	if !errors.Is(err, usecase.ErrKeyMismatch) {
		t.Fatalf("error = %v", err)
	}
	if got, _ := f.balance(t); got != "975.00" {
		t.Fatalf("debit applied twice: %s", got)
	}
}

func TestProviderCannotReplayAnotherProvidersKey(t *testing.T) {
	f := newFixture(t, "1000.00")
	if _, err := f.wagering.Process(context.Background(), f.input(t, "BET", "tx-1", "25.00")); err != nil {
		t.Fatal(err)
	}
	in := f.input(t, "BET", "tx-1", "25.00")
	in.ProviderID = "provider-b"
	_, err := f.wagering.Process(context.Background(), in)
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidationHappensBeforeAnyWrite(t *testing.T) {
	f := newFixture(t, "100.00")
	cases := map[string]func(*usecase.ProcessInput){
		"opening kind":       func(in *usecase.ProcessInput) { in.Kind = "OPENING" },
		"unknown kind":       func(in *usecase.ProcessInput) { in.Kind = "JACKPOT" },
		"missing key":        func(in *usecase.ProcessInput) { in.IdempotencyKey = "" },
		"missing round":      func(in *usecase.ProcessInput) { in.RoundID = "" },
		"bad wallet id":      func(in *usecase.ProcessInput) { in.WalletID = "nope" },
		"bet with zero":      func(in *usecase.ProcessInput) { in.Money = brl(t, "0.00") },
		"loss with amount":   func(in *usecase.ProcessInput) { in.Kind = "LOSS"; in.Money = brl(t, "1.00") },
		"refund without ref": func(in *usecase.ProcessInput) { in.Kind = "REFUND" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := f.input(t, "BET", "tx-"+name, "1.00")
			mutate(&in)
			_, err := f.wagering.Process(context.Background(), in)
			if !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if len(f.store.Transactions) != 1 {
		t.Fatalf("invalid input persisted transactions: %d", len(f.store.Transactions))
	}
}

func TestUnknownWalletIsNotFoundAndPersistsNothing(t *testing.T) {
	f := newFixture(t, "100.00")
	in := f.input(t, "BET", "tx-1", "1.00")
	in.WalletID = "0192f291-0000-7000-8000-000000000000"
	if _, err := f.wagering.Process(context.Background(), in); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
	if len(f.store.Transactions) != 1 {
		t.Fatalf("transaction persisted for unknown wallet")
	}
}

func TestWalletMismatchesAreRejections(t *testing.T) {
	f := newFixture(t, "100.00")
	in := f.input(t, "BET", "tx-1", "1.00")
	in.PlayerID = "0192f28f-0000-7000-8000-000000000009"
	res, err := f.wagering.Process(context.Background(), in)
	if err != nil || res.Transaction.FailureCode() != wagering.PlayerMismatch {
		t.Fatalf("player mismatch: %v %+v", err, res.Transaction.Snapshot())
	}

	in = f.input(t, "BET", "tx-2", "1.00")
	in.Money, _ = money.New(100, "USD")
	res, err = f.wagering.Process(context.Background(), in)
	if err != nil || res.Transaction.FailureCode() != wagering.CurrencyMismatch {
		t.Fatalf("currency mismatch: %v %+v", err, res.Transaction.Snapshot())
	}
}

func TestInfrastructureFailureRollsBackEverything(t *testing.T) {
	for _, method := range []string{"Ledger.Insert", "Wallets.UpdateBalance", "Transactions.Update", "Outbox.Insert"} {
		t.Run(method, func(t *testing.T) {
			f := newFixture(t, "100.00")
			f.store.FailOn, f.store.FailErr = method, errs.ErrTransient
			_, err := f.wagering.Process(context.Background(), f.input(t, "BET", "tx-1", "10.00"))
			if !errors.Is(err, errs.ErrTransient) {
				t.Fatalf("error = %v", err)
			}
			if got, v := f.balance(t); got != "100.00" || v != 1 {
				t.Fatalf("wallet changed: %s v%d", got, v)
			}
			if len(f.store.Transactions) != 1 || len(f.store.Ledger) != 1 {
				t.Fatalf("partial state: %d tx, %d ledger", len(f.store.Transactions), len(f.store.Ledger))
			}
		})
	}
}

func TestInboxRedeliveryReplaysWithoutReprocessing(t *testing.T) {
	f := newFixture(t, "100.00")
	in := f.input(t, "BET", "tx-1", "10.00")
	in.Inbox = &usecase.InboxMark{ConsumerName: "wager-consumer", MessageID: "msg-1", PayloadHash: []byte("h1"), ReceivedAt: fixedNow}

	first, err := f.wagering.Process(context.Background(), in)
	if err != nil || first.IdempotentReplay {
		t.Fatalf("first: %v %+v", err, first)
	}
	if _, found, _ := f.store.Repos().Inbox.Find(context.Background(), "wager-consumer", "msg-1"); !found {
		t.Fatalf("inbox row not written with the outcome")
	}

	again, err := f.wagering.Process(context.Background(), in)
	if err != nil || !again.IdempotentReplay || again.Transaction.ID() != first.Transaction.ID() {
		t.Fatalf("redelivery: %v %+v", err, again)
	}
	if got, _ := f.balance(t); got != "90.00" {
		t.Fatalf("redelivery moved money: %s", got)
	}

	// Same key, new message id: still a replay, and the new message is
	// recorded so its own redeliveries are deduplicated too.
	in.Inbox.MessageID = "msg-2"
	res, err := f.wagering.Process(context.Background(), in)
	if err != nil || !res.IdempotentReplay {
		t.Fatalf("new message id: %v %+v", err, res)
	}
	if _, found, _ := f.store.Repos().Inbox.Find(context.Background(), "wager-consumer", "msg-2"); !found {
		t.Fatalf("second message not recorded")
	}

	// A redelivered message id with different content is poison.
	in.Inbox.MessageID, in.Inbox.PayloadHash = "msg-1", []byte("changed")
	if _, err := f.wagering.Process(context.Background(), in); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("changed payload: %v", err)
	}
}

func TestTwoBetsRacingForTheSameBalance(t *testing.T) {
	f := newFixture(t, "100.00")
	var wg sync.WaitGroup
	results := make([]usecase.ProcessResult, 2)
	for i, external := range []string{"bet-a", "bet-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := f.wagering.Process(context.Background(), f.input(t, "BET", external, "80.00"))
			if err != nil {
				t.Error(err)
				return
			}
			results[i] = res
		}()
	}
	wg.Wait()

	processed, rejected := 0, 0
	for _, res := range results {
		switch res.Transaction.Status() {
		case wagering.Processed:
			processed++
		case wagering.Rejected:
			rejected++
			if res.Transaction.FailureCode() != wagering.InsufficientBalance {
				t.Errorf("code = %s", res.Transaction.FailureCode())
			}
		}
	}
	if processed != 1 || rejected != 1 {
		t.Fatalf("processed=%d rejected=%d", processed, rejected)
	}
	if got, v := f.balance(t); got != "20.00" || v != 2 {
		t.Fatalf("final balance %s v%d", got, v)
	}
	debits := 0
	for _, row := range f.store.Ledger {
		if row.Entry.Direction() == wallet.Debit {
			debits++
		}
	}
	if debits != 1 {
		t.Fatalf("debits = %d", debits)
	}
}

func TestFiftyParallelReplaysProduceOneDebit(t *testing.T) {
	f := newFixture(t, "100.00")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.wagering.Process(context.Background(), f.input(t, "BET", "same", "10.00")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got, v := f.balance(t); got != "90.00" || v != 2 {
		t.Fatalf("balance %s v%d", got, v)
	}
	if len(f.store.Ledger) != 2 || len(f.store.Transactions) != 2 {
		t.Fatalf("%d ledger entries, %d transactions", len(f.store.Ledger), len(f.store.Transactions))
	}
}
