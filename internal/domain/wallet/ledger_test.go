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

func params(t *testing.T, dir wallet.Direction, amount, before, after string) wallet.LedgerEntryParams {
	t.Helper()
	return wallet.LedgerEntryParams{
		ID: entryID, WalletID: walletID, TransactionID: txID, Direction: dir,
		Amount: brl(t, amount), BalanceBefore: brl(t, before), BalanceAfter: brl(t, after), CreatedAt: now,
	}
}

func TestNewLedgerEntryAcceptsConsistentArithmetic(t *testing.T) {
	for _, p := range []wallet.LedgerEntryParams{
		params(t, wallet.Credit, "25.00", "0.00", "25.00"),
		params(t, wallet.Debit, "25.00", "25.00", "0.00"),
		params(t, wallet.Debit, "0.01", "100.00", "99.99"),
	} {
		e, err := wallet.NewLedgerEntry(p)
		if err != nil {
			t.Fatalf("%s %s: %v", p.Direction, p.Amount, err)
		}
		if e.Direction() != p.Direction || !e.Amount().Equal(p.Amount) || !e.CreatedAt().Equal(now) {
			t.Fatalf("fields not kept: %+v", e)
		}
	}
}

func TestNewLedgerEntryRejectsInconsistentArithmetic(t *testing.T) {
	cases := []struct {
		name string
		p    wallet.LedgerEntryParams
		want error
	}{
		{"credit wrong after", params(t, wallet.Credit, "25.00", "0.00", "24.00"), wallet.ErrLedgerArithmetic},
		{"debit wrong after", params(t, wallet.Debit, "25.00", "25.00", "1.00"), wallet.ErrLedgerArithmetic},
		{"direction swapped", params(t, wallet.Debit, "25.00", "0.00", "25.00"), errs.ErrValidation},
		{"zero amount", params(t, wallet.Credit, "0.00", "1.00", "1.00"), errs.ErrValidation},
		{"unknown direction", params(t, "TRANSFER", "1.00", "0.00", "1.00"), errs.ErrValidation},
		{"zero time", func() wallet.LedgerEntryParams {
			p := params(t, wallet.Credit, "1.00", "0.00", "1.00")
			p.CreatedAt = time.Time{}
			return p
		}(), errs.ErrValidation},
		{"bad ids", func() wallet.LedgerEntryParams {
			p := params(t, wallet.Credit, "1.00", "0.00", "1.00")
			p.WalletID = "x"
			return p
		}(), id.ErrInvalidID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := wallet.NewLedgerEntry(tc.p); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewLedgerEntryRejectsNegativeBalancesAndMixedCurrencies(t *testing.T) {
	negative, _ := money.New(-100, money.BRL)
	p := params(t, wallet.Debit, "1.00", "0.00", "0.00")
	p.BalanceAfter = negative
	if _, err := wallet.NewLedgerEntry(p); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("negative after: %v", err)
	}

	usd, _ := money.New(100, "USD")
	p = params(t, wallet.Credit, "1.00", "0.00", "1.00")
	p.Amount = usd
	if _, err := wallet.NewLedgerEntry(p); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("mixed currencies: %v", err)
	}

	p = params(t, wallet.Credit, "1.00", "0.00", "1.00")
	p.Amount = money.Money{}
	if _, err := wallet.NewLedgerEntry(p); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("uninitialized amount: %v", err)
	}
}
