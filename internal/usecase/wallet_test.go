package usecase_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

const playerID = id.PlayerID("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")

var fixedNow = time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

func clock() time.Time { return fixedNow }

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func walletService(store *usecasetest.Store) *usecase.WalletService {
	return usecase.NewWalletService(store, store.Repos(), clock, quietLog())
}

func TestOpenWalletWithInitialBalanceCommitsEverythingTogether(t *testing.T) {
	store := usecasetest.NewStore()
	svc := walletService(store)

	w, err := svc.Open(context.Background(), usecase.OpenWalletInput{PlayerID: playerID, InitialBalance: brl(t, "1000.00"), CorrelationID: "corr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "1000.00" || w.Version() != 1 || w.PlayerID() != playerID {
		t.Fatalf("wallet: %+v", w.Snapshot())
	}

	if len(store.Transactions) != 1 {
		t.Fatalf("transactions = %d", len(store.Transactions))
	}
	var opening wagering.Snapshot
	for _, s := range store.Transactions {
		opening = s
	}
	if opening.Kind != wagering.Opening || opening.Origin != wagering.Internal || opening.Status != wagering.Processed ||
		opening.BalanceAfter.Amount() != "1000.00" || opening.WalletVersionAfter != 1 || opening.CorrelationID != "corr-1" {
		t.Fatalf("opening: %+v", opening)
	}

	if len(store.Ledger) != 1 || store.Ledger[0].Entry.Direction() != wallet.Credit || store.Ledger[0].Entry.TransactionID() != opening.ID {
		t.Fatalf("ledger: %+v", store.Ledger)
	}

	pending := store.PendingOutbox()
	if len(pending) != 2 {
		t.Fatalf("outbox = %d events", len(pending))
	}
	types := map[string]event.Envelope{}
	for _, rec := range pending {
		types[rec.Envelope.EventType] = rec.Envelope
	}
	processed, ok := types[event.TypeWagerTransactionProcessed]
	if !ok || processed.AggregateID != opening.ID.String() || processed.CorrelationID != "corr-1" || processed.CausationID != opening.ID.String() {
		t.Fatalf("processed event: %+v", processed)
	}
	changed, ok := types[event.TypeWalletBalanceChanged]
	if !ok || changed.AggregateID != w.ID().String() {
		t.Fatalf("balance changed event: %+v", changed)
	}
	if changed.EventID == processed.EventID {
		t.Fatalf("events must have distinct ids")
	}
}

func TestOpenWalletWithZeroBalanceCreatesOnlyTheWallet(t *testing.T) {
	store := usecasetest.NewStore()
	w, err := walletService(store).Open(context.Background(), usecase.OpenWalletInput{PlayerID: playerID, InitialBalance: brl(t, "0.00")})
	if err != nil {
		t.Fatal(err)
	}
	if !w.Balance().IsZero() || w.Version() != 1 {
		t.Fatalf("wallet: %+v", w.Snapshot())
	}
	if len(store.Transactions) != 0 || len(store.Ledger) != 0 || len(store.Outbox) != 0 {
		t.Fatalf("zero opening must not create transaction, ledger or events")
	}
	if _, ok := store.Wallets[w.ID()]; !ok {
		t.Fatalf("wallet not stored")
	}
}

func TestOpenWalletConflictsOnSamePlayerAndCurrency(t *testing.T) {
	store := usecasetest.NewStore()
	svc := walletService(store)
	if _, err := svc.Open(context.Background(), usecase.OpenWalletInput{PlayerID: playerID, InitialBalance: brl(t, "1.00")}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Open(context.Background(), usecase.OpenWalletInput{PlayerID: playerID, InitialBalance: brl(t, "5.00")})
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	if len(store.Wallets) != 1 || len(store.Transactions) != 1 || len(store.Ledger) != 1 || len(store.Outbox) != 2 {
		t.Fatalf("second attempt leaked state: %d wallets, %d tx, %d ledger, %d events",
			len(store.Wallets), len(store.Transactions), len(store.Ledger), len(store.Outbox))
	}

	usd, _ := money.New(100, "USD")
	if _, err := svc.Open(context.Background(), usecase.OpenWalletInput{PlayerID: playerID, InitialBalance: usd}); err != nil {
		t.Fatalf("same player, other currency must be allowed: %v", err)
	}
}

func TestOpenWalletRollsBackWhenAnyWriteFails(t *testing.T) {
	for _, method := range []string{"Transactions.Insert", "Ledger.Insert", "Outbox.Insert"} {
		t.Run(method, func(t *testing.T) {
			store := usecasetest.NewStore()
			store.FailOn, store.FailErr = method, errs.ErrTransient

			_, err := walletService(store).Open(context.Background(), usecase.OpenWalletInput{PlayerID: playerID, InitialBalance: brl(t, "10.00")})
			if !errors.Is(err, errs.ErrTransient) {
				t.Fatalf("error = %v", err)
			}
			if len(store.Wallets) != 0 || len(store.Transactions) != 0 || len(store.Ledger) != 0 || len(store.Outbox) != 0 {
				t.Fatalf("partial state persisted after failure in %s", method)
			}
		})
	}
}

func TestOpenWalletValidatesInput(t *testing.T) {
	svc := walletService(usecasetest.NewStore())
	if _, err := svc.Open(context.Background(), usecase.OpenWalletInput{PlayerID: "x", InitialBalance: brl(t, "1.00")}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("bad player id: %v", err)
	}
	if _, err := svc.Open(context.Background(), usecase.OpenWalletInput{PlayerID: playerID}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("uninitialized money: %v", err)
	}
	negative, _ := money.New(-1, money.BRL)
	if _, err := svc.Open(context.Background(), usecase.OpenWalletInput{PlayerID: playerID, InitialBalance: negative}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("negative: %v", err)
	}
}

func TestGetWallet(t *testing.T) {
	store := usecasetest.NewStore()
	svc := walletService(store)
	w, _ := svc.Open(context.Background(), usecase.OpenWalletInput{PlayerID: playerID, InitialBalance: brl(t, "3.00")})

	got, err := svc.Get(context.Background(), w.ID())
	if err != nil || got.Snapshot() != w.Snapshot() {
		t.Fatalf("get: %v", err)
	}
	if _, err := svc.Get(context.Background(), "0192f291-0000-7000-8000-000000000000"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := svc.Get(context.Background(), "nope"); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("bad id: %v", err)
	}
}

func TestListLedgerPagesWithOpaqueCursor(t *testing.T) {
	store := usecasetest.NewStore()
	svc := walletService(store)
	w, _ := svc.Open(context.Background(), usecase.OpenWalletInput{PlayerID: playerID, InitialBalance: brl(t, "100.00")})

	// Append four more entries straight into the store.
	snap := store.Wallets[w.ID()]
	agg, _ := wallet.Rehydrate(snap)
	for i := 0; i < 4; i++ {
		entry, err := agg.Debit(wallet.Movement{
			EntryID: id.LedgerEntryID(newUUIDForTest(i)), TransactionID: id.TransactionID(newUUIDForTest(10 + i)),
			Amount: brl(t, "1.00"), At: fixedNow,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Repos().Ledger.Insert(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}

	page1, err := svc.ListLedger(context.Background(), w.ID(), "", 2)
	if err != nil || len(page1.Entries) != 2 || page1.NextCursor == "" {
		t.Fatalf("page1: %v, %d entries, cursor %q", err, len(page1.Entries), page1.NextCursor)
	}
	if page1.Entries[0].Direction() != wallet.Credit {
		t.Fatalf("first entry must be the opening credit")
	}
	page2, err := svc.ListLedger(context.Background(), w.ID(), page1.NextCursor, 2)
	if err != nil || len(page2.Entries) != 2 || page2.NextCursor == "" {
		t.Fatalf("page2: %v, %d entries", err, len(page2.Entries))
	}
	page3, err := svc.ListLedger(context.Background(), w.ID(), page2.NextCursor, 2)
	if err != nil || len(page3.Entries) != 1 || page3.NextCursor != "" {
		t.Fatalf("page3: %v, %d entries, cursor %q", err, len(page3.Entries), page3.NextCursor)
	}
	seen := map[id.LedgerEntryID]bool{}
	for _, p := range []usecase.LedgerPage{page1, page2, page3} {
		for _, e := range p.Entries {
			if seen[e.ID()] {
				t.Fatalf("entry %s returned twice", e.ID())
			}
			seen[e.ID()] = true
		}
	}

	if _, err := svc.ListLedger(context.Background(), w.ID(), "not base64!", 2); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("bad cursor: %v", err)
	}
	if _, err := svc.ListLedger(context.Background(), "0192f291-0000-7000-8000-000000000000", "", 2); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown wallet: %v", err)
	}
	all, _ := svc.ListLedger(context.Background(), w.ID(), "", 0)
	if len(all.Entries) != 5 {
		t.Errorf("default limit: %d entries", len(all.Entries))
	}
}

func newUUIDForTest(n int) string {
	return "0192f2a0-0000-7000-8000-" + padHex(n)
}

func padHex(n int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 12)
	for i := 11; i >= 0; i-- {
		out[i] = digits[n%16]
		n /= 16
	}
	return string(out)
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
