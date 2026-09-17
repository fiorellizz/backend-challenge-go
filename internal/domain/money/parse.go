package money

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
)

var (
	// ErrInvalidAmount is returned for strings that are not plain decimals
	// with at most two fractional digits.
	ErrInvalidAmount = fmt.Errorf("%w: invalid amount", errs.ErrValidation)

	// ErrNegativeAmount is returned when an external input carries a sign.
	ErrNegativeAmount = fmt.Errorf("%w: negative amount", errs.ErrValidation)
)

// Parse builds a Money from the external decimal contract, e.g. "25.00".
//
// Accepted grammar: digits, optionally followed by "." and one or two digits.
// "25" and "25.5" are equivalent forms of "25.00" and "25.50"; that
// normalization happens here, before any hashing. Everything else is
// rejected: empty strings, signs, whitespace, exponents, "NaN", "Infinity",
// more than two fractional digits, or values outside the int64 range.
func Parse(amount string, currency string) (Money, error) {
	if strings.HasPrefix(amount, "-") {
		return Money{}, fmt.Errorf("%w: %q", ErrNegativeAmount, amount)
	}
	return ParseSigned(amount, currency)
}

// ParseSigned is Parse with an optional leading "-". It exists for internal
// values such as reconciliation differences and must not be used on
// financial input from providers.
func ParseSigned(amount string, currency string) (Money, error) {
	cur, err := ParseCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	minor, err := parseMinor(amount)
	if err != nil {
		return Money{}, err
	}
	return Money{minor: minor, currency: cur}, nil
}

func parseMinor(amount string) (int64, error) {
	s := amount
	negative := false
	if strings.HasPrefix(s, "-") {
		negative = true
		s = s[1:]
	}

	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if intPart == "" || !allDigits(intPart) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, amount)
	}
	if hasDot && (fracPart == "" || len(fracPart) > 2 || !allDigits(fracPart)) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, amount)
	}

	major, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrOverflow, amount)
	}
	cents, _ := strconv.ParseInt(padRight(fracPart, 2), 10, 64)

	if major > (math.MaxInt64-cents)/scale {
		return 0, fmt.Errorf("%w: %q", ErrOverflow, amount)
	}
	minor := major*scale + cents
	if negative {
		minor = -minor
	}
	return minor, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func padRight(s string, width int) string {
	for len(s) < width {
		s += "0"
	}
	return s
}

// Amount formats the value as a decimal string with exactly two fractional
// digits, e.g. "25.00" or "-0.50".
func (m Money) Amount() string {
	units := uint64(m.minor)
	sign := ""
	if m.minor < 0 {
		sign = "-"
		units = uint64(-(m.minor + 1)) + 1 // safe for MinInt64
	}
	return fmt.Sprintf("%s%d.%02d", sign, units/scale, units%scale)
}

// String renders the amount followed by its currency, for logs and errors.
func (m Money) String() string {
	return m.Amount() + " " + string(m.currency)
}

type moneyJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// MarshalJSON renders the external contract {"amount":"25.00","currency":"BRL"}.
func (m Money) MarshalJSON() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(moneyJSON{Amount: m.Amount(), Currency: string(m.currency)})
}

// UnmarshalJSON parses the external contract with the strict rules of Parse:
// negative amounts are rejected because no financial input may carry one.
func (m *Money) UnmarshalJSON(data []byte) error {
	var raw moneyJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAmount, err)
	}
	parsed, err := Parse(raw.Amount, raw.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
