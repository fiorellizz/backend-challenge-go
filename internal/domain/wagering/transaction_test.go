package wagering_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
)

func TestNewExternalStartsPending(t *testing.T) {
	tx := external(t, wagering.Bet, "25.00")
	if tx.Status() != wagering.Pending || tx.Origin() != wagering.External || tx.Kind() != wagering.Bet {
		t.Fatalf("unexpected transaction: %+v", tx.Snapshot())
	}
	if _, _, ok := tx.Result(); ok {
		t.Fatalf("pending transaction must not expose a result")
	}
	if tx.ProviderID() != "provider-a" || tx.IdempotencyKey() != "provider-a:transaction-123" || tx.RoundID() != "round-987" {
		t.Fatalf("metadata not kept: %+v", tx.Snapshot())
	}
}

func TestAmountPolicyPerKind(t *testing.T) {
	cases := []struct {
		kind   wagering.Kind
		amount string
		ok     bool
	}{
		{wagering.Bet, "0.01", true}, {wagering.Bet, "0.00", false},
		{wagering.Win, "1.00", true}, {wagering.Win, "0.00", false},
		{wagering.Loss, "0.00", true}, {wagering.Loss, "0.01", false},
		{wagering.Refund, "1.00", true}, {wagering.Refund, "0.00", false},
		{wagering.Rollback, "1.00", true}, {wagering.Rollback, "0.00", false},
	}
	for _, tc := range cases {
		_, err := wagering.NewExternal(externalParams(t, tc.kind, tc.amount), now)
		if tc.ok && err != nil {
			t.Errorf("%s %s: unexpected error %v", tc.kind, tc.amount, err)
		}
		if !tc.ok && !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s %s: error = %v, want validation", tc.kind, tc.amount, err)
		}
	}
}

func TestNewExternalRejectsOpeningAndReferenceMisuse(t *testing.T) {
	p := externalParams(t, wagering.Opening, "1.00")
	if _, err := wagering.NewExternal(p, now); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("OPENING from provider: %v", err)
	}

	p = externalParams(t, wagering.Refund, "1.00")
	p.ReferenceExternalTransactionID = ""
	if _, err := wagering.NewExternal(p, now); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("REFUND without reference: %v", err)
	}

	p = externalParams(t, wagering.Bet, "1.00")
	p.ReferenceExternalTransactionID = "transaction-100"
	if _, err := wagering.NewExternal(p, now); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("BET with reference: %v", err)
	}

	p = externalParams(t, wagering.Win, "1.00")
	p.ReferenceExternalTransactionID = "transaction-100"
	if _, err := wagering.NewExternal(p, now); err != nil {
		t.Errorf("WIN may reference a bet: %v", err)
	}

	p = externalParams(t, "JACKPOT", "1.00")
	if _, err := wagering.NewExternal(p, now); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("unknown kind: %v", err)
	}
}

func TestNewExternalRequiresProviderMetadata(t *testing.T) {
	mutations := map[string]func(*wagering.ExternalParams){
		"provider":  func(p *wagering.ExternalParams) { p.ProviderID = "" },
		"external":  func(p *wagering.ExternalParams) { p.ExternalTransactionID = "" },
		"key":       func(p *wagering.ExternalParams) { p.IdempotencyKey = "" },
		"hash":      func(p *wagering.ExternalParams) { p.PayloadHash = nil },
		"round":     func(p *wagering.ExternalParams) { p.RoundID = "" },
		"game":      func(p *wagering.ExternalParams) { p.GameID = "" },
		"wallet id": func(p *wagering.ExternalParams) { p.WalletID = "x" },
		"player id": func(p *wagering.ExternalParams) { p.PlayerID = "" },
	}
	for name, mutate := range mutations {
		p := externalParams(t, wagering.Bet, "1.00")
		mutate(&p)
		if _, err := wagering.NewExternal(p, now); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("missing %s: error = %v", name, err)
		}
	}
}

func TestNewOpeningIsInternalAndPositive(t *testing.T) {
	tx, err := wagering.NewOpening(wagering.OpeningParams{
		ID: txID, WalletID: walletID, PlayerID: playerID, Money: brl(t, "1000.00"), CorrelationID: "c",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Origin() != wagering.Internal || tx.Kind() != wagering.Opening || tx.Status() != wagering.Pending {
		t.Fatalf("unexpected opening: %+v", tx.Snapshot())
	}
	if tx.ProviderID() != "" || tx.IdempotencyKey() != "" || tx.PayloadHash() != nil {
		t.Fatalf("opening must not carry external metadata")
	}

	_, err = wagering.NewOpening(wagering.OpeningParams{ID: txID, WalletID: walletID, PlayerID: playerID, Money: brl(t, "0.00")}, now)
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("zero opening: %v", err)
	}
}

func TestRehydrateEnforcesShape(t *testing.T) {
	valid := external(t, wagering.Bet, "1.00").Snapshot()

	cases := []struct {
		name   string
		mutate func(*wagering.Snapshot)
	}{
		{"external opening", func(s *wagering.Snapshot) { s.Kind = wagering.Opening }},
		{"internal with provider", func(s *wagering.Snapshot) { s.Origin = wagering.Internal; s.Kind = wagering.Opening }},
		{"internal bet", func(s *wagering.Snapshot) {
			s.Origin = wagering.Internal
			s.ProviderID, s.ExternalTransactionID, s.IdempotencyKey, s.PayloadHash, s.RoundID, s.GameID = "", "", "", nil, "", ""
		}},
		{"unknown origin", func(s *wagering.Snapshot) { s.Origin = "SIDE" }},
		{"unknown status", func(s *wagering.Snapshot) { s.Status = "DONE" }},
		{"rejected without code", func(s *wagering.Snapshot) { s.Status = wagering.Rejected }},
		{"processed without result", func(s *wagering.Snapshot) { s.Status = wagering.Processed }},
		{"pending reference on bet", func(s *wagering.Snapshot) { s.Status = wagering.PendingReference }},
		{"negative amount", func(s *wagering.Snapshot) { s.Money, _ = s.Money.Neg() }},
		{"no timestamps", func(s *wagering.Snapshot) { s.CreatedAt = time.Time{} }},
		{"bad id", func(s *wagering.Snapshot) { s.ID = "nope" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := valid
			tc.mutate(&s)
			if _, err := wagering.Rehydrate(s); !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}

	processed := external(t, wagering.Bet, "1.00")
	if err := processed.MarkProcessed(brl(t, "99.00"), 2, now); err != nil {
		t.Fatal(err)
	}
	back, err := wagering.Rehydrate(processed.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if balance, version, ok := back.Result(); !ok || balance.Amount() != "99.00" || version != 2 {
		t.Fatalf("rehydrated result = %s, %d, %v", balance, version, ok)
	}
}
