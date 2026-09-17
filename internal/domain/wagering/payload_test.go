package wagering_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
)

func payload(t *testing.T, amount string) wagering.Payload {
	t.Helper()
	return wagering.Payload{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-123",
		PlayerID: playerID.String(), WalletID: walletID.String(),
		RoundID: "round-987", GameID: "fortune-chimp", Kind: wagering.Bet, Money: brl(t, amount),
	}
}

func TestPayloadHashIsDeterministicAndNormalized(t *testing.T) {
	a, err := payload(t, "25.00").Hash()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := payload(t, "25").Hash()
	c, _ := payload(t, "25.0").Hash()
	if !bytes.Equal(a, b) || !bytes.Equal(a, c) {
		t.Fatalf("equivalent amounts must hash equally")
	}
	if len(a) != 32 {
		t.Fatalf("hash length = %d, want 32 (sha256)", len(a))
	}
}

func TestPayloadHashChangesWithAnyBusinessField(t *testing.T) {
	base, _ := payload(t, "25.00").Hash()
	mutations := map[string]func(*wagering.Payload){
		"amount":    func(p *wagering.Payload) { p.Money = brl(t, "25.01") },
		"currency":  func(p *wagering.Payload) { p.Money, _ = money.New(2500, "USD") },
		"kind":      func(p *wagering.Payload) { p.Kind = wagering.Win },
		"round":     func(p *wagering.Payload) { p.RoundID = "round-1" },
		"game":      func(p *wagering.Payload) { p.GameID = "other" },
		"player":    func(p *wagering.Payload) { p.PlayerID = "x" },
		"wallet":    func(p *wagering.Payload) { p.WalletID = "x" },
		"provider":  func(p *wagering.Payload) { p.ProviderID = "provider-b" },
		"external":  func(p *wagering.Payload) { p.ExternalTransactionID = "transaction-124" },
		"reference": func(p *wagering.Payload) { p.ReferenceExternalTransactionID = "transaction-100" },
	}
	for name, mutate := range mutations {
		p := payload(t, "25.00")
		mutate(&p)
		h, err := p.Hash()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bytes.Equal(h, base) {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
}

func TestPayloadHashRejectsUninitializedMoney(t *testing.T) {
	p := payload(t, "1.00")
	p.Money = money.Money{}
	if _, err := p.Hash(); !errors.Is(err, money.ErrUninitialized) {
		t.Fatalf("error = %v", err)
	}
}
