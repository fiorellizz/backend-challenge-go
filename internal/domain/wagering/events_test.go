package wagering_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/event"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
)

var meta = event.Meta{EventID: "evt-1", CorrelationID: "corr-1", CausationID: "cause-1", OccurredAt: now.In(time.FixedZone("BRT", -3*3600))}

func TestProcessedEventCarriesResultAndUTC(t *testing.T) {
	tx := external(t, wagering.Bet, "25.00")
	if _, err := tx.ProcessedEvent(meta); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("event before processing: %v", err)
	}
	_ = tx.MarkProcessed(brl(t, "975.00"), 2, now)

	env, err := tx.ProcessedEvent(meta)
	if err != nil {
		t.Fatal(err)
	}
	if env.EventType != event.TypeWagerTransactionProcessed || env.Version != 1 || env.AggregateID != txID.String() || env.AggregateType != event.AggregateWagerTransaction {
		t.Fatalf("envelope: %+v", env)
	}
	if env.OccurredAt.Location() != time.UTC {
		t.Fatalf("occurredAt not UTC: %s", env.OccurredAt)
	}

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"eventId":"evt-1"`, `"correlationId":"corr-1"`, `"causationId":"cause-1"`,
		`"occurredAt":"2026-09-17T12:00:00Z"`, `"balance":{"amount":"975.00","currency":"BRL"}`,
		`"walletVersion":2`, `"kind":"BET"`, `"providerId":"provider-a"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("json missing %s:\n%s", want, raw)
		}
	}
}

func TestOpeningProcessedEventOmitsProviderFields(t *testing.T) {
	tx, _ := wagering.NewOpening(wagering.OpeningParams{ID: txID, WalletID: walletID, PlayerID: playerID, Money: brl(t, "10.00"), CorrelationID: "c"}, now)
	_ = tx.MarkProcessed(brl(t, "10.00"), 1, now)
	env, err := tx.ProcessedEvent(meta)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(env)
	for _, absent := range []string{"providerId", "externalTransactionId", "roundId", "gameId"} {
		if strings.Contains(string(raw), absent) {
			t.Errorf("opening event must not carry %s", absent)
		}
	}
	if !strings.Contains(string(raw), `"kind":"OPENING"`) {
		t.Errorf("kind missing: %s", raw)
	}
}

func TestRejectedAndPendingReferenceEvents(t *testing.T) {
	tx := external(t, wagering.Refund, "25.00")
	if _, err := tx.RejectedEvent(meta); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("rejected event while pending: %v", err)
	}
	_ = tx.AwaitReference(policy, now)
	env, err := tx.PendingReferenceEvent(meta)
	if err != nil {
		t.Fatal(err)
	}
	data := env.Data.(event.WagerTransactionPendingReference)
	if data.ReferenceExternalTransactionID != "transaction-100" || !data.NextAttemptAt.Equal(now.Add(2*time.Second)) || data.DeadlineAt.Location() != time.UTC {
		t.Fatalf("pending data: %+v", data)
	}

	_ = tx.MarkRejected(wagering.ReferenceNotFound, now)
	env, err = tx.RejectedEvent(meta)
	if err != nil {
		t.Fatal(err)
	}
	if env.EventType != event.TypeWagerTransactionRejected || env.Data.(event.WagerTransactionRejected).FailureCode != "REFERENCE_NOT_FOUND" {
		t.Fatalf("rejected envelope: %+v", env)
	}
	if _, err := tx.PendingReferenceEvent(meta); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("pending event after rejection: %v", err)
	}
}

func TestBalanceChangedEvent(t *testing.T) {
	w := openWallet(t, "100.00")
	entry, err := w.Debit(wallet.Movement{EntryID: entryID, TransactionID: txID, Amount: brl(t, "80.00"), At: now})
	if err != nil {
		t.Fatal(err)
	}
	env, err := wagering.BalanceChangedEvent(meta, entry, w.Version())
	if err != nil {
		t.Fatal(err)
	}
	if env.EventType != event.TypeWalletBalanceChanged || env.AggregateType != event.AggregateWallet || env.AggregateID != walletID.String() {
		t.Fatalf("envelope: %+v", env)
	}
	data := env.Data.(event.WalletBalanceChanged)
	if data.Direction != "DEBIT" || data.BalanceBefore.Amount() != "100.00" || data.BalanceAfter.Amount() != "20.00" || data.WalletVersion != 2 {
		t.Fatalf("data: %+v", data)
	}
}

func TestEventMetaValidation(t *testing.T) {
	tx := external(t, wagering.Bet, "1.00")
	_ = tx.MarkProcessed(brl(t, "1.00"), 1, now)
	for name, m := range map[string]event.Meta{
		"no id":          {CorrelationID: "c", OccurredAt: now},
		"no correlation": {EventID: "e", OccurredAt: now},
		"no time":        {EventID: "e", CorrelationID: "c"},
	} {
		if _, err := tx.ProcessedEvent(m); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
