package id_test

import (
	"errors"
	"testing"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
)

func TestParseAcceptsCanonicalUUIDs(t *testing.T) {
	for _, s := range []string{
		"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
		"0192F28F-5DC0-7D58-BDB2-814AD6A0F4A1",
		"00000000-0000-0000-0000-000000000000",
	} {
		if _, err := id.ParseWalletID(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestParseRejectsMalformedIDs(t *testing.T) {
	for _, s := range []string{
		"",
		"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a",
		"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a12",
		"0192f28f5dc07d58bdb2814ad6a0f4a1",
		"0192f28f-5dc0-7d58-bdb2-814ad6a0f4g1",
		"{0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1}",
	} {
		_, err := id.ParsePlayerID(s)
		if !errors.Is(err, id.ErrInvalidID) || !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%q: error = %v", s, err)
		}
	}
}

func TestEachTypeValidatesItself(t *testing.T) {
	if err := id.TransactionID("nope").Validate(); !errors.Is(err, id.ErrInvalidID) {
		t.Errorf("TransactionID: %v", err)
	}
	if err := id.LedgerEntryID("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1").Validate(); err != nil {
		t.Errorf("LedgerEntryID: %v", err)
	}
	if id.WalletID("abc").String() != "abc" {
		t.Errorf("String must return the raw value")
	}
}
