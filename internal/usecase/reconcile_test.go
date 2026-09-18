package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
)

func TestReconcileConsistentWallet(t *testing.T) {
	f := newFixture(t, "1000.00")
	f.process(t, f.input(t, "BET", "bet-1", "25.00"))
	f.process(t, f.input(t, "WIN", "win-1", "5.00"))
	f.process(t, f.input(t, "LOSS", "loss-1", "0.00"))
	f.process(t, f.input(t, "BET", "bet-big", "5000.00")) // rejected, no entry

	rec, err := f.wallets.Reconcile(context.Background(), f.walletID)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Consistent || rec.StoredBalance.Amount() != "980.00" || rec.CalculatedBalance.Amount() != "980.00" ||
		rec.Difference.Amount() != "0.00" || rec.CheckedEntries != 3 {
		t.Fatalf("reconciliation: %+v", rec)
	}
}

func TestReconcileDetectsDivergenceWithoutChangingAnything(t *testing.T) {
	f := newFixture(t, "100.00")
	var observedWallet id.WalletID
	var observedDiff money.Money
	f.wallets.WithDivergenceObserver(func(w id.WalletID, d money.Money) { observedWallet, observedDiff = w, d })

	// Corrupt the stored balance behind the aggregate's back.
	snap := f.store.Wallets[f.walletID]
	snap.Balance, _ = money.New(9500, money.BRL)
	f.store.Wallets[f.walletID] = snap

	rec, err := f.wallets.Reconcile(context.Background(), f.walletID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Consistent || rec.StoredBalance.Amount() != "95.00" || rec.CalculatedBalance.Amount() != "100.00" || rec.Difference.Amount() != "-5.00" {
		t.Fatalf("reconciliation: %+v", rec)
	}
	if observedWallet != f.walletID || observedDiff.Amount() != "-5.00" {
		t.Fatalf("observer not notified: %s %s", observedWallet, observedDiff)
	}
	if f.store.Wallets[f.walletID].Balance.Amount() != "95.00" || len(f.store.Ledger) != 1 {
		t.Fatalf("reconciliation changed state")
	}
}

func TestReconcileZeroWalletAndErrors(t *testing.T) {
	f := newFixture(t, "0.00")
	rec, err := f.wallets.Reconcile(context.Background(), f.walletID)
	if err != nil || !rec.Consistent || rec.CheckedEntries != 0 || rec.Difference.Amount() != "0.00" {
		t.Fatalf("zero wallet: %+v %v", rec, err)
	}
	if _, err := f.wallets.Reconcile(context.Background(), "0192f291-0000-7000-8000-000000000000"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := f.wallets.Reconcile(context.Background(), "nope"); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("bad id: %v", err)
	}
}
