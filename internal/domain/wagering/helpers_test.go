package wagering_test

import (
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
)

const (
	walletID = id.WalletID("0192f291-27dd-7d3f-8071-5f8685deef37")
	playerID = id.PlayerID("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	txID     = id.TransactionID("0192f298-345e-7e38-af88-e43f851a819d")
	refID    = id.TransactionID("0192f298-0000-7000-8000-000000000001")
	entryID  = id.LedgerEntryID("0192f2a0-0000-7000-8000-000000000001")
)

var (
	now    = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	policy = wagering.ReferencePolicy{BaseBackoff: 2 * time.Second, MaxAttempts: 4, TTL: time.Minute}
)

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func externalParams(t *testing.T, kind wagering.Kind, amount string) wagering.ExternalParams {
	t.Helper()
	p := wagering.ExternalParams{
		ID: txID, ProviderID: "provider-a", ExternalTransactionID: "transaction-123",
		IdempotencyKey: "provider-a:transaction-123", PayloadHash: []byte{1, 2, 3},
		WalletID: walletID, PlayerID: playerID, RoundID: "round-987", GameID: "fortune-chimp",
		Kind: kind, Money: brl(t, amount), CorrelationID: "corr-1",
	}
	if kind.IsReversal() {
		p.ReferenceExternalTransactionID = "transaction-100"
	}
	return p
}

func external(t *testing.T, kind wagering.Kind, amount string) *wagering.WagerTransaction {
	t.Helper()
	tx, err := wagering.NewExternal(externalParams(t, kind, amount), now)
	if err != nil {
		t.Fatalf("NewExternal(%s %s): %v", kind, amount, err)
	}
	return tx
}

// processedReference builds a PROCESSED transaction that reversals in the
// tests point to: external id "transaction-100", same provider/player/
// wallet/round as external().
func processedReference(t *testing.T, kind wagering.Kind, amount string) *wagering.WagerTransaction {
	t.Helper()
	p := externalParams(t, kind, amount)
	p.ID = refID
	p.ExternalTransactionID = "transaction-100"
	p.IdempotencyKey = "provider-a:transaction-100"
	p.ReferenceExternalTransactionID = ""
	if kind.IsReversal() {
		p.ReferenceExternalTransactionID = "transaction-050"
	}
	ref, err := wagering.NewExternal(p, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := ref.MarkProcessed(brl(t, "100.00"), 2, now); err != nil {
		t.Fatal(err)
	}
	return ref
}

func openWallet(t *testing.T, balance string) *wallet.Wallet {
	t.Helper()
	w, err := wallet.New(walletID, playerID, money.BRL, now)
	if err != nil {
		t.Fatal(err)
	}
	if balance != "0.00" {
		_, err = w.OpeningCredit(wallet.Movement{EntryID: entryID, TransactionID: refID, Amount: brl(t, balance), At: now})
		if err != nil {
			t.Fatal(err)
		}
	}
	return w
}
