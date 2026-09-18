package worker_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase/usecasetest"
	"github.com/fiorellizz/backend-challenge-go/internal/worker"
)

const playerID = id.PlayerID("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")

func TestRunOnceDrainsEveryDueReference(t *testing.T) {
	store := usecasetest.NewStore()
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	policy := wagering.ReferencePolicy{BaseBackoff: time.Second, MaxAttempts: 5, TTL: time.Minute}
	wallets := usecase.NewWalletService(store, store.Repos(), clock)
	svc, err := usecase.NewWageringService(store, store.Repos(), clock, policy)
	if err != nil {
		t.Fatal(err)
	}
	amount, _ := money.Parse("10.00", "BRL")
	initial, _ := money.Parse("30.00", "BRL")
	w, err := wallets.Open(context.Background(), usecase.OpenWalletInput{PlayerID: playerID, InitialBalance: initial})
	if err != nil {
		t.Fatal(err)
	}

	// Three refunds arrive before their bets.
	for _, n := range []string{"a", "b", "c"} {
		_, err := svc.Process(context.Background(), usecase.ProcessInput{
			IdempotencyKey: "provider-a:refund-" + n, ProviderID: "provider-a", ExternalTransactionID: "refund-" + n,
			PlayerID: playerID.String(), WalletID: w.ID().String(), RoundID: "r", GameID: "g", Kind: "REFUND",
			Money: amount, ReferenceExternalTransactionID: "bet-" + n,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Then the bets arrive.
	for _, n := range []string{"a", "b", "c"} {
		_, err := svc.Process(context.Background(), usecase.ProcessInput{
			IdempotencyKey: "provider-a:bet-" + n, ProviderID: "provider-a", ExternalTransactionID: "bet-" + n,
			PlayerID: playerID.String(), WalletID: w.ID().String(), RoundID: "r", GameID: "g", Kind: "BET", Money: amount,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	resolver := worker.NewReferenceResolver(svc, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))

	handled, err := resolver.RunOnce(context.Background())
	if err != nil || handled != 0 {
		t.Fatalf("before backoff: handled %d, %v", handled, err)
	}

	now = now.Add(time.Second)
	handled, err = resolver.RunOnce(context.Background())
	if err != nil || handled != 3 {
		t.Fatalf("after backoff: handled %d, %v", handled, err)
	}
	for _, s := range store.Transactions {
		if s.Kind == wagering.Refund && s.Status != wagering.Processed {
			t.Fatalf("refund %s: %s", s.ExternalTransactionID, s.Status)
		}
	}
	got, _ := wallets.Get(context.Background(), w.ID())
	if got.Balance().Amount() != "30.00" {
		t.Fatalf("balance %s", got.Balance())
	}

	// Run returns promptly when cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		resolver.Run(ctx)
		close(done)
	}()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}
