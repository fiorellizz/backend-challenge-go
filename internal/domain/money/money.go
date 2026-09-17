// Package money implements an immutable monetary value object.
//
// Representation: an int64 count of minor units (cents) with a fixed scale of
// two decimal places, plus an ISO 4217 currency. Floating point is never used.
// The representable range is therefore ±92,233,720,368,547,758.07 and every
// operation that could leave it reports ErrOverflow instead of wrapping.
//
// The zero value Money{} carries no currency and is rejected by every method.
package money

import (
	"fmt"
	"math"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
)

// scale is the number of minor units in one major unit (two decimal places).
const scale = 100

var (
	// ErrUninitialized is returned when a zero-value Money is used.
	ErrUninitialized = fmt.Errorf("%w: uninitialized money", errs.ErrValidation)

	// ErrCurrencyMismatch is returned when two values of different currencies
	// are combined or compared.
	ErrCurrencyMismatch = fmt.Errorf("%w: currency mismatch", errs.ErrValidation)

	// ErrOverflow is returned when a result cannot be represented in int64.
	ErrOverflow = fmt.Errorf("%w: amount overflow", errs.ErrValidation)
)

// Money is an immutable amount in a single currency. Values are compared and
// combined only with values of the same currency.
type Money struct {
	minor    int64
	currency Currency
}

// New builds a Money from minor units. It is the rehydration path used by the
// persistence layer and allows negative amounts, which are valid for internal
// differences but never for wallet balances.
func New(minor int64, currency Currency) (Money, error) {
	if err := currency.Validate(); err != nil {
		return Money{}, err
	}
	return Money{minor: minor, currency: currency}, nil
}

// Zero returns the zero amount of the given currency.
func Zero(currency Currency) (Money, error) {
	return New(0, currency)
}

// Minor returns the amount in minor units.
func (m Money) Minor() int64 { return m.minor }

// Currency returns the currency of the amount.
func (m Money) Currency() Currency { return m.currency }

// IsZero reports whether the amount is exactly zero.
func (m Money) IsZero() bool { return m.minor == 0 }

// IsPositive reports whether the amount is strictly greater than zero.
func (m Money) IsPositive() bool { return m.minor > 0 }

// IsNegative reports whether the amount is strictly less than zero.
func (m Money) IsNegative() bool { return m.minor < 0 }

// Validate rejects the zero value and malformed currencies.
func (m Money) Validate() error {
	if m.currency == "" {
		return ErrUninitialized
	}
	return m.currency.Validate()
}

// Add returns m + o.
func (m Money) Add(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, err
	}
	if (o.minor > 0 && m.minor > math.MaxInt64-o.minor) ||
		(o.minor < 0 && m.minor < math.MinInt64-o.minor) {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrOverflow, m, o)
	}
	return Money{minor: m.minor + o.minor, currency: m.currency}, nil
}

// Sub returns m - o.
func (m Money) Sub(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, err
	}
	if (o.minor < 0 && m.minor > math.MaxInt64+o.minor) ||
		(o.minor > 0 && m.minor < math.MinInt64+o.minor) {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrOverflow, m, o)
	}
	return Money{minor: m.minor - o.minor, currency: m.currency}, nil
}

// Neg returns -m.
func (m Money) Neg() (Money, error) {
	if err := m.Validate(); err != nil {
		return Money{}, err
	}
	if m.minor == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: negating %s", ErrOverflow, m)
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

// Compare returns -1, 0 or 1 as m is less than, equal to or greater than o.
func (m Money) Compare(o Money) (int, error) {
	if err := m.sameCurrency(o); err != nil {
		return 0, err
	}
	switch {
	case m.minor < o.minor:
		return -1, nil
	case m.minor > o.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal reports whether both values have the same currency and amount.
// Different currencies are simply not equal; no error is raised.
func (m Money) Equal(o Money) bool {
	return m.currency == o.currency && m.minor == o.minor
}

func (m Money) sameCurrency(o Money) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if err := o.Validate(); err != nil {
		return err
	}
	if m.currency != o.currency {
		return fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	return nil
}
