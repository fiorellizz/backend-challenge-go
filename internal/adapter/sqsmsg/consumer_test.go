package sqsmsg_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/sqsmsg"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase/usecasetest"
)

const playerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"

type harness struct {
	store    *usecasetest.Store
	consumer *sqsmsg.Consumer
	walletID string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := usecasetest.NewStore()
	clock := func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }
	wallets := usecase.NewWalletService(store, store.Repos(), clock, quietLog())
	svc, err := usecase.NewWageringService(store, store.Repos(), clock, wagering.ReferencePolicy{BaseBackoff: time.Second, MaxAttempts: 3, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	initial, _ := money.Parse("100.00", "BRL")
	w, err := wallets.Open(context.Background(), usecase.OpenWalletInput{PlayerID: id.PlayerID(playerID), InitialBalance: initial})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	consumer := sqsmsg.NewConsumer(nil, config.Config{SQS: config.SQS{VisibilityTimeoutSeconds: 30}}, svc, clock, log)
	return &harness{store: store, consumer: consumer, walletID: w.ID().String()}
}

func (h *harness) message(messageID, external, kind, amount string) string {
	return fmt.Sprintf(`{"messageId":"%s","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z","data":{"providerId":"provider-a","externalTransactionId":"%s","idempotencyKey":"provider-a:%s","playerId":"%s","walletId":"%s","roundId":"round-987","gameId":"fortune-chimp","kind":"%s","money":{"amount":"%s","currency":"BRL"}}}`,
		messageID, external, external, playerID, h.walletID, kind, amount)
}

func TestHandleProcessesAndAcks(t *testing.T) {
	h := newHarness(t)
	out := h.consumer.Handle(context.Background(), h.message("msg-1", "tx-1", "BET", "25.00"))
	if out.Decision != sqsmsg.Ack || out.MessageID != "msg-1" || out.Result.Transaction.Status() != wagering.Processed {
		t.Fatalf("handled = %+v", out)
	}
	if _, found, _ := h.store.Repos().Inbox.Find(context.Background(), sqsmsg.ConsumerName, "msg-1"); !found {
		t.Fatalf("inbox row missing")
	}
	w, _ := h.store.Repos().Wallets.Get(context.Background(), id.WalletID(h.walletID))
	if w.Balance().Amount() != "75.00" {
		t.Fatalf("balance %s", w.Balance())
	}
}

func TestRedeliveryIsReplayedNotReapplied(t *testing.T) {
	h := newHarness(t)
	body := h.message("msg-1", "tx-1", "BET", "25.00")
	first := h.consumer.Handle(context.Background(), body)
	again := h.consumer.Handle(context.Background(), body)
	if again.Decision != sqsmsg.Ack || !again.Result.IdempotentReplay || again.Result.Transaction.ID() != first.Result.Transaction.ID() {
		t.Fatalf("redelivery = %+v", again)
	}
	w, _ := h.store.Repos().Wallets.Get(context.Background(), id.WalletID(h.walletID))
	if w.Balance().Amount() != "75.00" || len(h.store.Ledger) != 2 {
		t.Fatalf("redelivery moved money: %s, %d entries", w.Balance(), len(h.store.Ledger))
	}
}

func TestBusinessRejectionIsAcked(t *testing.T) {
	h := newHarness(t)
	out := h.consumer.Handle(context.Background(), h.message("msg-1", "tx-1", "BET", "500.00"))
	if out.Decision != sqsmsg.Ack || out.Result.Transaction.Status() != wagering.Rejected || out.Result.Transaction.FailureCode() != wagering.InsufficientBalance {
		t.Fatalf("handled = %+v", out)
	}
}

func TestPermanentProblemsAreRejectedToDLQ(t *testing.T) {
	h := newHarness(t)
	cases := map[string]string{
		"malformed json":  `{"messageId":`,
		"missing id":      `{"type":"WagerTransactionRequested","data":{}}`,
		"wrong type":      `{"messageId":"m","type":"SomethingElse","data":{}}`,
		"opening kind":    h.message("m1", "x", "OPENING", "1.00"),
		"invalid amount":  h.message("m2", "x", "BET", "1e3"),
		"loss with money": h.message("m3", "x", "LOSS", "1.00"),
		"unknown wallet":  fmt.Sprintf(`{"messageId":"m4","type":"WagerTransactionRequested","data":{"providerId":"provider-a","externalTransactionId":"x","idempotencyKey":"k","playerId":"%s","walletId":"0192f291-0000-7000-8000-000000000000","roundId":"r","gameId":"g","kind":"BET","money":{"amount":"1.00","currency":"BRL"}}}`, playerID),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out := h.consumer.Handle(context.Background(), body)
			if out.Decision != sqsmsg.Reject || out.Reason == "" {
				t.Fatalf("handled = %+v", out)
			}
		})
	}
	if len(h.store.Transactions) != 1 {
		t.Fatalf("rejected messages persisted transactions: %d", len(h.store.Transactions))
	}

	// Same key, different payload: a conflict is permanent too.
	h.consumer.Handle(context.Background(), h.message("m5", "tx-5", "BET", "1.00"))
	out := h.consumer.Handle(context.Background(), h.message("m6", "tx-5", "BET", "2.00"))
	if out.Decision != sqsmsg.Reject {
		t.Fatalf("conflict = %+v", out)
	}
	// Same message id redelivered with a different body is poison.
	out = h.consumer.Handle(context.Background(), h.message("m5", "tx-5", "WIN", "1.00"))
	if out.Decision != sqsmsg.Reject {
		t.Fatalf("changed body = %+v", out)
	}
}

func TestTransientFailureIsLeftForRedelivery(t *testing.T) {
	h := newHarness(t)
	h.store.FailOn, h.store.FailErr = "Ledger.Insert", errs.ErrTransient
	out := h.consumer.Handle(context.Background(), h.message("msg-1", "tx-1", "BET", "25.00"))
	if out.Decision != sqsmsg.Retry {
		t.Fatalf("handled = %+v", out)
	}
	if _, found, _ := h.store.Repos().Inbox.Find(context.Background(), sqsmsg.ConsumerName, "msg-1"); found {
		t.Fatalf("inbox row written despite rollback")
	}
	if len(h.store.Transactions) != 1 {
		t.Fatalf("transaction persisted despite rollback")
	}
	// After the outage the same delivery succeeds.
	h.store.FailOn = ""
	if out := h.consumer.Handle(context.Background(), h.message("msg-1", "tx-1", "BET", "25.00")); out.Decision != sqsmsg.Ack || out.Result.IdempotentReplay {
		t.Fatalf("recovery = %+v", out)
	}
}

func TestPendingReferenceIsAcked(t *testing.T) {
	h := newHarness(t)
	body := fmt.Sprintf(`{"messageId":"m-ref","type":"WagerTransactionRequested","data":{"providerId":"provider-a","externalTransactionId":"refund-1","idempotencyKey":"provider-a:refund-1","playerId":"%s","walletId":"%s","roundId":"round-987","gameId":"g","kind":"REFUND","money":{"amount":"1.00","currency":"BRL"},"referenceExternalTransactionId":"bet-late"}}`, playerID, h.walletID)
	out := h.consumer.Handle(context.Background(), body)
	if out.Decision != sqsmsg.Ack || out.Result.Transaction.Status() != wagering.PendingReference {
		t.Fatalf("handled = %+v", out)
	}
	if _, found, _ := h.store.Repos().Inbox.Find(context.Background(), sqsmsg.ConsumerName, "m-ref"); !found {
		t.Fatalf("pending reference must complete the inbox row; the worker continues")
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
