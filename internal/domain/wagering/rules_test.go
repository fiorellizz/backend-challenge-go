package wagering_test

import (
	"errors"
	"testing"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
)

func TestEffectPerKind(t *testing.T) {
	cases := []struct {
		name      string
		tx        *wagering.WagerTransaction
		ref       *wagering.WagerTransaction
		direction wallet.Direction
		moves     bool
	}{
		{"bet debits", external(t, wagering.Bet, "25.00"), nil, wallet.Debit, true},
		{"win credits", external(t, wagering.Win, "25.00"), nil, wallet.Credit, true},
		{"loss moves nothing", external(t, wagering.Loss, "0.00"), nil, "", false},
		{"refund of bet credits", external(t, wagering.Refund, "25.00"), processedReference(t, wagering.Bet, "25.00"), wallet.Credit, true},
		{"rollback of bet credits", external(t, wagering.Rollback, "25.00"), processedReference(t, wagering.Bet, "25.00"), wallet.Credit, true},
		{"rollback of win debits", external(t, wagering.Rollback, "25.00"), processedReference(t, wagering.Win, "25.00"), wallet.Debit, true},
		{"rollback of refund debits", external(t, wagering.Rollback, "25.00"), processedReference(t, wagering.Refund, "25.00"), wallet.Debit, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			effect, moves, err := tc.tx.Effect(tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			if moves != tc.moves {
				t.Fatalf("moves = %v, want %v", moves, tc.moves)
			}
			if moves && (effect.Direction != tc.direction || !effect.Amount.Equal(tc.tx.Money())) {
				t.Fatalf("effect = %+v", effect)
			}
		})
	}
}

func TestOpeningEffectCredits(t *testing.T) {
	tx, _ := wagering.NewOpening(wagering.OpeningParams{ID: txID, WalletID: walletID, PlayerID: playerID, Money: brl(t, "10.00")}, now)
	effect, moves, err := tx.Effect(nil)
	if err != nil || !moves || effect.Direction != wallet.Credit {
		t.Fatalf("effect = %+v, %v, %v", effect, moves, err)
	}
}

func TestReversalReferenceRules(t *testing.T) {
	cases := []struct {
		name string
		kind wagering.Kind
		ref  func() *wagering.WagerTransaction
		code wagering.FailureCode
	}{
		{"refund of win", wagering.Refund, func() *wagering.WagerTransaction { return processedReference(t, wagering.Win, "25.00") }, wagering.ReferenceMismatch},
		{"refund of loss", wagering.Refund, func() *wagering.WagerTransaction { return processedReference(t, wagering.Loss, "0.00") }, wagering.ReferenceMismatch},
		{"rollback of rollback", wagering.Rollback, func() *wagering.WagerTransaction { return processedReference(t, wagering.Rollback, "25.00") }, wagering.ReferenceMismatch},
		{"amount differs", wagering.Refund, func() *wagering.WagerTransaction { return processedReference(t, wagering.Bet, "20.00") }, wagering.ReferenceMismatch},
		{"other round", wagering.Refund, func() *wagering.WagerTransaction {
			p := externalParams(t, wagering.Bet, "25.00")
			p.ID, p.ExternalTransactionID, p.IdempotencyKey, p.RoundID = refID, "transaction-100", "k", "round-1"
			ref, _ := wagering.NewExternal(p, now)
			_ = ref.MarkProcessed(brl(t, "1.00"), 1, now)
			return ref
		}, wagering.ReferenceMismatch},
		{"other provider", wagering.Refund, func() *wagering.WagerTransaction {
			p := externalParams(t, wagering.Bet, "25.00")
			p.ID, p.ExternalTransactionID, p.IdempotencyKey, p.ProviderID = refID, "transaction-100", "k", "provider-b"
			ref, _ := wagering.NewExternal(p, now)
			_ = ref.MarkProcessed(brl(t, "1.00"), 1, now)
			return ref
		}, wagering.ReferenceMismatch},
		{"other external id", wagering.Refund, func() *wagering.WagerTransaction {
			p := externalParams(t, wagering.Bet, "25.00")
			p.ID, p.ExternalTransactionID, p.IdempotencyKey = refID, "transaction-999", "k"
			ref, _ := wagering.NewExternal(p, now)
			_ = ref.MarkProcessed(brl(t, "1.00"), 1, now)
			return ref
		}, wagering.ReferenceMismatch},
		{"other wallet", wagering.Rollback, func() *wagering.WagerTransaction {
			p := externalParams(t, wagering.Bet, "25.00")
			p.ID, p.ExternalTransactionID, p.IdempotencyKey = refID, "transaction-100", "k"
			p.WalletID = "0192f291-0000-7000-8000-000000000009"
			ref, _ := wagering.NewExternal(p, now)
			_ = ref.MarkProcessed(brl(t, "1.00"), 1, now)
			return ref
		}, wagering.ReferenceMismatch},
		{"reference still pending", wagering.Refund, func() *wagering.WagerTransaction {
			p := externalParams(t, wagering.Bet, "25.00")
			p.ID, p.ExternalTransactionID, p.IdempotencyKey = refID, "transaction-100", "k"
			ref, _ := wagering.NewExternal(p, now)
			return ref
		}, wagering.ReferenceNotProcessed},
		{"reference rejected", wagering.Rollback, func() *wagering.WagerTransaction {
			p := externalParams(t, wagering.Bet, "25.00")
			p.ID, p.ExternalTransactionID, p.IdempotencyKey = refID, "transaction-100", "k"
			ref, _ := wagering.NewExternal(p, now)
			_ = ref.MarkRejected(wagering.InsufficientBalance, now)
			return ref
		}, wagering.ReferenceNotProcessed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := external(t, tc.kind, "25.00").Effect(tc.ref())
			r, ok := wagering.AsRejection(err)
			if !ok || r.Code != tc.code {
				t.Fatalf("error = %v, want rejection %s", err, tc.code)
			}
		})
	}

	if _, _, err := external(t, wagering.Refund, "25.00").Effect(nil); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("nil reference must be a programming error, got %v", err)
	}
}

func TestCheckWallet(t *testing.T) {
	tx := external(t, wagering.Bet, "25.00")
	if err := tx.CheckWallet(openWallet(t, "100.00")); err != nil {
		t.Fatalf("matching wallet: %v", err)
	}

	usdWallet, _ := wallet.New(walletID, playerID, "USD", now)
	if r, ok := wagering.AsRejection(tx.CheckWallet(usdWallet)); !ok || r.Code != wagering.CurrencyMismatch {
		t.Errorf("currency: %v", r)
	}

	otherPlayer, _ := wallet.New(walletID, "0192f28f-0000-7000-8000-000000000002", money.BRL, now)
	if r, ok := wagering.AsRejection(tx.CheckWallet(otherPlayer)); !ok || r.Code != wagering.PlayerMismatch {
		t.Errorf("player: %v", r)
	}

	otherWallet, _ := wallet.New("0192f291-0000-7000-8000-000000000009", playerID, money.BRL, now)
	if err := tx.CheckWallet(otherWallet); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("wallet id: %v", err)
	}
}

func TestRejectionForMapsWalletErrors(t *testing.T) {
	w := openWallet(t, "100.00")
	mv := wallet.Movement{EntryID: entryID, TransactionID: txID, Amount: brl(t, "150.00"), At: now}
	_, debitErr := w.Debit(mv)

	if r, ok := external(t, wagering.Bet, "150.00").RejectionFor(debitErr); !ok || r.Code != wagering.InsufficientBalance {
		t.Errorf("bet: %v", r)
	}
	if r, ok := external(t, wagering.Rollback, "150.00").RejectionFor(debitErr); !ok || r.Code != wagering.ReversalInsufficientBalance {
		t.Errorf("rollback: %v", r)
	}

	usd, _ := money.New(1, "USD")
	_, curErr := w.Credit(wallet.Movement{EntryID: entryID, TransactionID: txID, Amount: usd, At: now})
	if r, ok := external(t, wagering.Win, "1.00").RejectionFor(curErr); !ok || r.Code != wagering.CurrencyMismatch {
		t.Errorf("currency: %v", r)
	}

	if _, ok := external(t, wagering.Win, "1.00").RejectionFor(errors.New("db down")); ok {
		t.Errorf("unknown errors must not become rejections")
	}
	if r, ok := external(t, wagering.Win, "1.00").RejectionFor(&wagering.Rejection{Code: wagering.ReferenceMismatch}); !ok || r.Code != wagering.ReferenceMismatch {
		t.Errorf("rejections pass through: %v", r)
	}
}

func TestFullBetFlowAgainstWallet(t *testing.T) {
	w := openWallet(t, "100.00")
	tx := external(t, wagering.Bet, "80.00")
	if err := tx.CheckWallet(w); err != nil {
		t.Fatal(err)
	}
	effect, _, _ := tx.Effect(nil)
	entry, err := w.Debit(wallet.Movement{EntryID: entryID, TransactionID: tx.ID(), Amount: effect.Amount, At: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.MarkProcessed(entry.BalanceAfter(), w.Version(), now); err != nil {
		t.Fatal(err)
	}
	balance, version, _ := tx.Result()
	if balance.Amount() != "20.00" || version != 2 {
		t.Fatalf("result = %s v%d", balance, version)
	}

	second := external(t, wagering.Bet, "80.00")
	effect, _, _ = second.Effect(nil)
	_, err = w.Debit(wallet.Movement{EntryID: entryID, TransactionID: second.ID(), Amount: effect.Amount, At: now})
	r, ok := second.RejectionFor(err)
	if !ok || r.Code != wagering.InsufficientBalance {
		t.Fatalf("second bet: %v", err)
	}
	if err := second.MarkRejected(r.Code, now); err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "20.00" || w.Version() != 2 {
		t.Fatalf("wallet after rejected bet: %s v%d", w.Balance(), w.Version())
	}
}
