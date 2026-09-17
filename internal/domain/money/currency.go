package money

import (
	"fmt"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
)

// Currency is an ISO 4217 alphabetic code such as "BRL". Only the shape is
// validated (three ASCII uppercase letters); the service does not keep a
// registry of known currencies.
type Currency string

// BRL is the currency used by every main scenario of the challenge.
const BRL Currency = "BRL"

// ParseCurrency validates the shape of an ISO 4217 code.
func ParseCurrency(code string) (Currency, error) {
	c := Currency(code)
	if err := c.Validate(); err != nil {
		return "", err
	}
	return c, nil
}

// Validate reports whether the code has the ISO 4217 alphabetic shape.
func (c Currency) Validate() error {
	if len(c) != 3 {
		return fmt.Errorf("%w: currency %q must have three letters", ErrInvalidCurrency, string(c))
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return fmt.Errorf("%w: currency %q must be uppercase ASCII letters", ErrInvalidCurrency, string(c))
		}
	}
	return nil
}

// String returns the code itself.
func (c Currency) String() string { return string(c) }

// ErrInvalidCurrency is returned for codes that are not three uppercase
// ASCII letters.
var ErrInvalidCurrency = fmt.Errorf("%w: invalid currency", errs.ErrValidation)
