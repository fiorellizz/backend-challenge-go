package wallet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
)

const (
	walletID = id.WalletID("0192f291-27dd-7d3f-8071-5f8685deef37")
	playerID = id.PlayerID("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	txID     = id.TransactionID("0192f298-345e-7e38-af88-e43f851a819d")
	entryID  = id.LedgerEntryID("0192f2a0-0000-7000-8000-000000000001")
)

var now = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func movement(t *testing.T, amount string) wallet.Movement {
	t.Helper()
	return wallet.Movement{EntryID: entryID, TransactionID: txID, Amount: brl(t, amount), At: now.Add(time.Minute)}
}

func newWallet(t *testing.T) *wallet.Wallet {
	t.Helper()
	w, err := wallet.New(walletID, playerID, money.BRL, now)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestNewStartsEmptyAtVersionOne(t *testing.T) {
	w := newWallet(t)
	if !w.Balance().IsZero() || w.Version() != 1 || w.Currency() != money.BRL {
		t.Fatalf("unexpected wallet: %+v", w.Snapshot())
	}
	if w.ID() != walletID || w.PlayerID() != playerID || !w.CreatedAt().Equal(now) {
		t.Fatalf("identity not kept: %+v", w.Snapshot())
	}
}

func TestNewRejectsInvalidInput(t *testing.T) {
	if _, err := wallet.New("bad", playerID, money.BRL, now); !errors.Is(err, id.ErrInvalidID) {
		t.Errorf("wallet id: %v", err)
	}
	if _, err := wallet.New(walletID, "bad", money.BRL, now); !errors.Is(err, id.ErrInvalidID) {
		t.Errorf("player id: %v", err)
	}
	if _, err := wallet.New(walletID, playerID, "br", now); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Errorf("currency: %v", err)
	}
	if _, err := wallet.New(walletID, playerID, money.BRL, time.Time{}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("zero time: %v", err)
	}
}

func TestOpeningCreditKeepsVersionOneAndRunsOnce(t *testing.T) {
	w := newWallet(t)

	entry, err := w.OpeningCredit(movement(t, "1000.00"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "1000.00" || w.Version() != 1 {
		t.Fatalf("balance = %s, version = %d", w.Balance(), w.Version())
	}
	if entry.Direction() != wallet.Credit || entry.BalanceBefore().Amount() != "0.00" || entry.BalanceAfter().Amount() != "1000.00" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if _, err := w.OpeningCredit(movement(t, "1.00")); !errors.Is(err, wallet.ErrAlreadyOpened) {
		t.Fatalf("second opening: %v", err)
	}
}

func TestRehydratedWalletRefusesOpeningCredit(t *testing.T) {
	w, err := wallet.Rehydrate(newWallet(t).Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.OpeningCredit(movement(t, "1.00")); !errors.Is(err, wallet.ErrAlreadyOpened) {
		t.Fatalf("error = %v, want ErrAlreadyOpened", err)
	}
	if _, err := w.Credit(movement(t, "1.00")); err != nil {
		t.Fatalf("regular credit must still work: %v", err)
	}
}

func TestDebitAndCreditBumpVersionAndProduceEntries(t *testing.T) {
	w := newWallet(t)
	if _, err := w.OpeningCredit(movement(t, "100.00")); err != nil {
		t.Fatal(err)
	}

	debit, err := w.Debit(movement(t, "80.00"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "20.00" || w.Version() != 2 {
		t.Fatalf("after debit: balance = %s, version = %d", w.Balance(), w.Version())
	}
	if debit.Direction() != wallet.Debit || debit.BalanceBefore().Amount() != "100.00" || debit.BalanceAfter().Amount() != "20.00" {
		t.Fatalf("unexpected debit entry: %+v", debit)
	}
	if debit.WalletID() != walletID || debit.TransactionID() != txID || debit.ID() != entryID {
		t.Fatalf("entry identity: %+v", debit)
	}

	credit, err := w.Credit(movement(t, "5.50"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "25.50" || w.Version() != 3 || credit.Amount().Amount() != "5.50" {
		t.Fatalf("after credit: balance = %s, version = %d", w.Balance(), w.Version())
	}
	if !w.UpdatedAt().Equal(now.Add(time.Minute)) {
		t.Fatalf("updatedAt not moved: %s", w.UpdatedAt())
	}
}

func TestDebitBeyondBalanceIsRejectedAndLeavesWalletUntouched(t *testing.T) {
	w := newWallet(t)
	if _, err := w.OpeningCredit(movement(t, "100.00")); err != nil {
		t.Fatal(err)
	}
	before := w.Snapshot()

	_, err := w.Debit(movement(t, "100.01"))
	if !errors.Is(err, wallet.ErrInsufficientBalance) {
		t.Fatalf("error = %v, want ErrInsufficientBalance", err)
	}
	if errors.Is(err, errs.ErrValidation) {
		t.Fatalf("insufficient balance is a business rejection, not validation")
	}
	if w.Snapshot() != before {
		t.Fatalf("wallet changed after rejected debit: %+v", w.Snapshot())
	}

	if _, err := w.Debit(movement(t, "100.00")); err != nil || !w.Balance().IsZero() {
		t.Fatalf("debit to exactly zero must succeed: %v, balance %s", err, w.Balance())
	}
}

func TestMovementValidation(t *testing.T) {
	w := newWallet(t)
	usd, _ := money.New(100, "USD")

	cases := []struct {
		name string
		m    wallet.Movement
		want error
	}{
		{"zero amount", movement(t, "0.00"), errs.ErrValidation},
		{"uninitialized amount", wallet.Movement{EntryID: entryID, TransactionID: txID, At: now}, money.ErrUninitialized},
		{"other currency", wallet.Movement{EntryID: entryID, TransactionID: txID, Amount: usd, At: now}, money.ErrCurrencyMismatch},
		{"bad entry id", wallet.Movement{EntryID: "x", TransactionID: txID, Amount: brl(t, "1.00"), At: now}, id.ErrInvalidID},
		{"bad transaction id", wallet.Movement{EntryID: entryID, TransactionID: "x", Amount: brl(t, "1.00"), At: now}, id.ErrInvalidID},
		{"zero time", wallet.Movement{EntryID: entryID, TransactionID: txID, Amount: brl(t, "1.00")}, errs.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.Credit(tc.m); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if w.Version() != 1 || !w.Balance().IsZero() {
				t.Fatalf("wallet mutated on invalid movement")
			}
		})
	}
}

func TestRehydrateValidatesInvariants(t *testing.T) {
	valid := newWallet(t).Snapshot()
	negative, _ := money.New(-1, money.BRL)
	usd, _ := money.New(0, "USD")

	cases := []struct {
		name   string
		mutate func(*wallet.Snapshot)
		want   error
	}{
		{"negative balance", func(s *wallet.Snapshot) { s.Balance = negative }, errs.ErrValidation},
		{"balance in another currency", func(s *wallet.Snapshot) { s.Balance = usd }, money.ErrCurrencyMismatch},
		{"version zero", func(s *wallet.Snapshot) { s.Version = 0 }, errs.ErrValidation},
		{"uninitialized balance", func(s *wallet.Snapshot) { s.Balance = money.Money{} }, money.ErrUninitialized},
		{"missing timestamps", func(s *wallet.Snapshot) { s.UpdatedAt = time.Time{} }, errs.ErrValidation},
		{"bad id", func(s *wallet.Snapshot) { s.ID = "nope" }, id.ErrInvalidID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := valid
			tc.mutate(&s)
			if _, err := wallet.Rehydrate(s); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}

	w, err := wallet.Rehydrate(wallet.Snapshot{
		ID: walletID, PlayerID: playerID, Currency: money.BRL, Balance: brl(t, "42.00"),
		Version: 7, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "42.00" || w.Version() != 7 {
		t.Fatalf("rehydration replayed something: %+v", w.Snapshot())
	}
}
