//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/postgres"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/event"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

func newUUID() string { return uuid.Must(uuid.NewV7()).String() }

func setup(t *testing.T) (*pgxpool.Pool, *postgres.UnitOfWork, usecase.Repositories) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	cfg := config.Config{Database: config.Database{URL: url, MaxConns: 5}}
	pool, err := postgres.NewPool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	return pool, postgres.NewUnitOfWork(pool), postgres.NewRepositories(pool)
}

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func openedWallet(t *testing.T, uow *postgres.UnitOfWork, balance string) (*wallet.Wallet, *wagering.WagerTransaction) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	w, err := wallet.New(id.WalletID(newUUID()), id.PlayerID(newUUID()), money.BRL, now)
	if err != nil {
		t.Fatal(err)
	}
	opening, err := wagering.NewOpening(wagering.OpeningParams{ID: id.TransactionID(newUUID()), WalletID: w.ID(), PlayerID: w.PlayerID(), Money: brl(t, balance), CorrelationID: "test"}, now)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := w.OpeningCredit(wallet.Movement{EntryID: id.LedgerEntryID(newUUID()), TransactionID: opening.ID(), Amount: brl(t, balance), At: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := opening.MarkProcessed(entry.BalanceAfter(), w.Version(), now); err != nil {
		t.Fatal(err)
	}
	err = uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error {
		if err := r.Wallets.Insert(ctx, w); err != nil {
			return err
		}
		if _, err := r.Transactions.Insert(ctx, opening); err != nil {
			return err
		}
		return r.Ledger.Insert(ctx, entry)
	})
	if err != nil {
		t.Fatal(err)
	}
	return w, opening
}

func externalBet(t *testing.T, w *wallet.Wallet, external string, amount string) *wagering.WagerTransaction {
	t.Helper()
	p := wagering.Payload{ProviderID: "provider-a", ExternalTransactionID: external, PlayerID: w.PlayerID().String(),
		WalletID: w.ID().String(), RoundID: "round-1", GameID: "game", Kind: wagering.Bet, Money: brl(t, amount)}
	hash, _ := p.Hash()
	tx, err := wagering.NewExternal(wagering.ExternalParams{
		ID: id.TransactionID(newUUID()), ProviderID: "provider-a", ExternalTransactionID: external,
		IdempotencyKey: "provider-a:" + external, PayloadHash: hash, WalletID: w.ID(), PlayerID: w.PlayerID(),
		RoundID: "round-1", GameID: "game", Kind: wagering.Bet, Money: brl(t, amount), CorrelationID: "corr",
	}, time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestWalletRoundTripAndUniqueness(t *testing.T) {
	_, uow, repos := setup(t)
	w, _ := openedWallet(t, uow, "100.00")

	got, err := repos.Wallets.Get(t.Context(), w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.Snapshot() != w.Snapshot() {
		t.Fatalf("round trip mismatch:\n%+v\n%+v", got.Snapshot(), w.Snapshot())
	}
	if _, err := repos.Wallets.Get(t.Context(), id.WalletID(newUUID())); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("missing wallet: %v", err)
	}

	dup, _ := wallet.New(id.WalletID(newUUID()), w.PlayerID(), money.BRL, time.Now())
	err = uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error { return r.Wallets.Insert(ctx, dup) })
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("second wallet for same player+currency: %v", err)
	}
}

func TestUpdateBalanceGuardsVersion(t *testing.T) {
	_, uow, repos := setup(t)
	w, _ := openedWallet(t, uow, "100.00")

	err := uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error {
		locked, err := r.Wallets.GetForUpdate(ctx, w.ID())
		if err != nil {
			return err
		}
		before := locked.Version()
		if _, err := locked.Debit(wallet.Movement{EntryID: id.LedgerEntryID(newUUID()), TransactionID: id.TransactionID(newUUID()), Amount: brl(t, "30.00"), At: time.Now()}); err != nil {
			return err
		}
		return r.Wallets.UpdateBalance(ctx, locked, before)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := repos.Wallets.Get(t.Context(), w.ID())
	if got.Balance().Amount() != "70.00" || got.Version() != 2 {
		t.Fatalf("after debit: %s v%d", got.Balance(), got.Version())
	}

	stale := uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error {
		return r.Wallets.UpdateBalance(ctx, got, 1) // version is 2 now
	})
	if !errors.Is(stale, errs.ErrConflict) {
		t.Fatalf("stale version: %v", stale)
	}
}

func TestForUpdateSerializesSameWalletOnly(t *testing.T) {
	_, uow, _ := setup(t)
	w, _ := openedWallet(t, uow, "100.00")
	other, _ := openedWallet(t, uow, "100.00")

	locked := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = uow.WithinTx(context.Background(), func(ctx context.Context, r usecase.Repositories) error {
			if _, err := r.Wallets.GetForUpdate(ctx, w.ID()); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	// Another wallet is not blocked.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := uow.WithinTx(ctx, func(ctx context.Context, r usecase.Repositories) error {
		_, err := r.Wallets.GetForUpdate(ctx, other.ID())
		return err
	}); err != nil {
		t.Fatalf("other wallet blocked: %v", err)
	}

	// The same wallet is blocked until the holder commits.
	ctx2, cancel2 := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel2()
	err := uow.WithinTx(ctx2, func(ctx context.Context, r usecase.Repositories) error {
		_, err := r.Wallets.GetForUpdate(ctx, w.ID())
		return err
	})
	if err == nil {
		t.Fatal("same wallet lock was not held")
	}
	close(release)
}

func TestTransactionInsertIsIdempotent(t *testing.T) {
	_, uow, repos := setup(t)
	w, _ := openedWallet(t, uow, "100.00")
	external := "tx-" + newUUID()
	first := externalBet(t, w, external, "25.00")

	var inserted bool
	err := uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error {
		var err error
		inserted, err = r.Transactions.Insert(ctx, first)
		return err
	})
	if err != nil || !inserted {
		t.Fatalf("first insert: %v, inserted %v", err, inserted)
	}

	// Same key again: not inserted, no error.
	again := externalBet(t, w, external, "25.00")
	err = uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error {
		var err error
		inserted, err = r.Transactions.Insert(ctx, again)
		return err
	})
	if err != nil || inserted {
		t.Fatalf("duplicate insert: %v, inserted %v", err, inserted)
	}

	byKey, err := repos.Transactions.GetByIdempotencyKey(t.Context(), "provider-a:"+external)
	if err != nil || byKey.ID() != first.ID() {
		t.Fatalf("by key: %v", err)
	}
	byExternal, err := repos.Transactions.GetByProviderExternalID(t.Context(), "provider-a", external)
	if err != nil || byExternal.ID() != first.ID() {
		t.Fatalf("by external: %v", err)
	}
	if byKey.Snapshot().CreatedAt.IsZero() || len(byKey.PayloadHash()) != 32 || byKey.Status() != wagering.Pending {
		t.Fatalf("rehydrated: %+v", byKey.Snapshot())
	}
}

func TestTransactionUpdateAndResult(t *testing.T) {
	_, uow, repos := setup(t)
	w, opening := openedWallet(t, uow, "100.00")

	got, err := repos.Transactions.GetByID(t.Context(), opening.ID())
	if err != nil {
		t.Fatal(err)
	}
	balance, version, ok := got.Result()
	if !ok || balance.Amount() != "100.00" || version != 1 || got.Origin() != wagering.Internal {
		t.Fatalf("opening result: %s v%d %v", balance, version, ok)
	}

	tx := externalBet(t, w, "tx-"+newUUID(), "10.00")
	now := time.Now().UTC().Truncate(time.Microsecond)
	err = uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error {
		if _, err := r.Transactions.Insert(ctx, tx); err != nil {
			return err
		}
		if err := tx.MarkRejected(wagering.InsufficientBalance, now); err != nil {
			return err
		}
		return r.Transactions.Update(ctx, tx)
	})
	if err != nil {
		t.Fatal(err)
	}
	back, _ := repos.Transactions.GetByID(t.Context(), tx.ID())
	if back.Status() != wagering.Rejected || back.FailureCode() != wagering.InsufficientBalance {
		t.Fatalf("rejected round trip: %+v", back.Snapshot())
	}
}

func TestLedgerListSumAndImmutability(t *testing.T) {
	pool, uow, repos := setup(t)
	w, opening := openedWallet(t, uow, "100.00")

	rows, err := repos.Ledger.List(t.Context(), w.ID(), 0, 10)
	if err != nil || len(rows) != 1 || rows[0].Entry.TransactionID() != opening.ID() {
		t.Fatalf("list: %v, %d rows", err, len(rows))
	}
	minor, count, err := repos.Ledger.Sum(t.Context(), w.ID())
	if err != nil || minor != 10000 || count != 1 {
		t.Fatalf("sum = %d over %d: %v", minor, count, err)
	}

	// Same transaction twice is refused by the unique constraint.
	dupEntry, _ := wallet.NewLedgerEntry(wallet.LedgerEntryParams{
		ID: id.LedgerEntryID(newUUID()), WalletID: w.ID(), TransactionID: opening.ID(), Direction: wallet.Credit,
		Amount: brl(t, "1.00"), BalanceBefore: brl(t, "100.00"), BalanceAfter: brl(t, "101.00"), CreatedAt: time.Now(),
	})
	err = uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error { return r.Ledger.Insert(ctx, dupEntry) })
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("duplicate ledger entry: %v", err)
	}

	// The table refuses mutation regardless of who asks.
	if _, err := pool.Exec(t.Context(), `UPDATE wallet_ledger_entries SET amount_minor = 1 WHERE wallet_id = $1`, w.ID().String()); err == nil {
		t.Fatal("ledger UPDATE was allowed")
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM wallet_ledger_entries WHERE wallet_id = $1`, w.ID().String()); err == nil {
		t.Fatal("ledger DELETE was allowed")
	}
}

func TestInboxFindAndInsert(t *testing.T) {
	_, uow, repos := setup(t)
	msgID := "msg-" + newUUID()

	_, found, err := repos.Inbox.Find(t.Context(), "wager-consumer", msgID)
	if err != nil || found {
		t.Fatalf("before insert: %v, found %v", err, found)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	msg := usecase.InboxMessage{ConsumerName: "wager-consumer", MessageID: msgID, PayloadHash: []byte{1, 2}, ReceivedAt: now, CompletedAt: now}
	if err := uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error { return r.Inbox.Insert(ctx, msg) }); err != nil {
		t.Fatal(err)
	}
	got, found, err := repos.Inbox.Find(t.Context(), "wager-consumer", msgID)
	if err != nil || !found || string(got.PayloadHash) != "\x01\x02" || !got.CompletedAt.Equal(now) {
		t.Fatalf("after insert: %v, found %v, %+v", err, found, got)
	}
	err = uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error { return r.Inbox.Insert(ctx, msg) })
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("duplicate inbox: %v", err)
	}
}

func TestOutboxClaimSkipsLockedRows(t *testing.T) {
	_, uow, repos := setup(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	aggregate := newUUID()

	err := uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error {
		for i := 0; i < 2; i++ {
			env, err := event.NewWalletBalanceChanged(
				event.Meta{EventID: newUUID(), CorrelationID: "corr", OccurredAt: now.Add(-time.Minute)},
				event.WalletBalanceChanged{WalletID: aggregate, TransactionID: newUUID(), Direction: "CREDIT",
					Money: brl(t, "1.00"), BalanceBefore: brl(t, "0.00"), BalanceAfter: brl(t, "1.00"), WalletVersion: 1})
			if err != nil {
				return err
			}
			if err := r.Outbox.Insert(ctx, env); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var firstClaim []usecase.OutboxRecord
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = uow.WithinTx(context.Background(), func(ctx context.Context, r usecase.Repositories) error {
			recs, err := r.Outbox.ClaimPending(ctx, 100, time.Now())
			firstClaim = recs
			close(held)
			<-release
			for _, rec := range recs {
				if err := r.Outbox.MarkPublished(ctx, rec.Envelope.EventID, time.Now()); err != nil {
					return err
				}
			}
			return err
		})
	}()
	<-held

	// A second publisher sees none of the rows the first one holds.
	err = uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error {
		recs, err := r.Outbox.ClaimPending(ctx, 100, time.Now())
		if err != nil {
			return err
		}
		for _, rec := range recs {
			for _, mine := range firstClaim {
				if rec.Envelope.EventID == mine.Envelope.EventID {
					t.Errorf("event %s claimed twice", rec.Envelope.EventID)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for {
		lag, err := repos.Outbox.OldestPendingAge(t.Context(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if lag < time.Minute || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	claimed := false
	for _, rec := range firstClaim {
		if rec.Envelope.AggregateID == aggregate {
			claimed = true
			if rec.Envelope.EventType != event.TypeWalletBalanceChanged || len(rec.Data) == 0 {
				t.Fatalf("record: %+v", rec)
			}
		}
	}
	if !claimed {
		t.Fatal("first publisher did not claim the inserted events")
	}
}

func TestSnapshotTransactionIsReadOnly(t *testing.T) {
	_, uow, _ := setup(t)
	w, _ := openedWallet(t, uow, "1.00")
	err := uow.WithinSnapshot(t.Context(), func(ctx context.Context, r usecase.Repositories) error {
		return r.Wallets.UpdateBalance(ctx, w, 1)
	})
	if err == nil {
		t.Fatal("write inside a read-only snapshot succeeded")
	}
}

func TestErrorsAreClassified(t *testing.T) {
	_, uow, repos := setup(t)
	if _, err := repos.Transactions.GetByID(t.Context(), id.TransactionID(newUUID())); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("not found: %v", err)
	}
	unsaved, _ := wallet.New(id.WalletID(newUUID()), id.PlayerID(newUUID()), money.BRL, time.Now())
	err := uow.WithinTx(t.Context(), func(ctx context.Context, r usecase.Repositories) error {
		return r.Transactions.Update(ctx, externalBet(t, unsaved, "x", "1.00"))
	})
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}

	unreachable, err := postgres.NewPool(config.Config{Database: config.Database{URL: "postgres://nobody:x@127.0.0.1:1/none", MaxConns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer unreachable.Close()
	_, err = postgres.NewRepositories(unreachable).Wallets.Get(t.Context(), id.WalletID(newUUID()))
	if !errors.Is(err, errs.ErrTransient) {
		t.Errorf("unreachable database: %v", err)
	}
}
