// Package id defines the identity types shared across domain aggregates.
// Each one is a UUID in canonical textual form. The domain only validates
// the shape; generating new identifiers is the application layer's job.
package id

import (
	"fmt"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
)

// ErrInvalidID is returned for identifiers that are not canonical UUIDs.
var ErrInvalidID = fmt.Errorf("%w: invalid id", errs.ErrValidation)

type (
	// WalletID identifies a wallet aggregate.
	WalletID string
	// PlayerID identifies the owner of one wallet per currency.
	PlayerID string
	// TransactionID identifies a wager transaction, internal or external.
	TransactionID string
	// LedgerEntryID identifies one immutable ledger entry.
	LedgerEntryID string
)

// ParseWalletID validates the canonical UUID shape.
func ParseWalletID(s string) (WalletID, error) { return parse[WalletID](s) }

// ParsePlayerID validates the canonical UUID shape.
func ParsePlayerID(s string) (PlayerID, error) { return parse[PlayerID](s) }

// ParseTransactionID validates the canonical UUID shape.
func ParseTransactionID(s string) (TransactionID, error) { return parse[TransactionID](s) }

// ParseLedgerEntryID validates the canonical UUID shape.
func ParseLedgerEntryID(s string) (LedgerEntryID, error) { return parse[LedgerEntryID](s) }

func (v WalletID) String() string      { return string(v) }
func (v PlayerID) String() string      { return string(v) }
func (v TransactionID) String() string { return string(v) }
func (v LedgerEntryID) String() string { return string(v) }

// Validate reports whether the identifier has the canonical UUID shape.
func (v WalletID) Validate() error      { return validate(string(v)) }
func (v PlayerID) Validate() error      { return validate(string(v)) }
func (v TransactionID) Validate() error { return validate(string(v)) }
func (v LedgerEntryID) Validate() error { return validate(string(v)) }

func parse[T ~string](s string) (T, error) {
	if err := validate(s); err != nil {
		return "", err
	}
	return T(s), nil
}

// validate accepts 8-4-4-4-12 hexadecimal groups, any case.
func validate(s string) error {
	if len(s) != 36 {
		return fmt.Errorf("%w: %q", ErrInvalidID, s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return fmt.Errorf("%w: %q", ErrInvalidID, s)
			}
		default:
			if !isHex(c) {
				return fmt.Errorf("%w: %q", ErrInvalidID, s)
			}
		}
	}
	return nil
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
