package money_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
)

func TestParseAcceptsDecimalForms(t *testing.T) {
	cases := []struct {
		in    string
		minor int64
		out   string
	}{
		{"0", 0, "0.00"},
		{"0.00", 0, "0.00"},
		{"25.00", 2500, "25.00"},
		{"25", 2500, "25.00"},
		{"25.5", 2550, "25.50"},
		{"0.01", 1, "0.01"},
		{"007.10", 710, "7.10"},
		{"92233720368547758.07", math.MaxInt64, "92233720368547758.07"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			m, err := money.Parse(tc.in, "BRL")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if m.Minor() != tc.minor {
				t.Fatalf("minor = %d, want %d", m.Minor(), tc.minor)
			}
			if m.Amount() != tc.out {
				t.Fatalf("amount = %q, want %q", m.Amount(), tc.out)
			}
			if m.Currency() != money.BRL {
				t.Fatalf("currency = %q, want BRL", m.Currency())
			}
		})
	}
}

func TestParseRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", money.ErrInvalidAmount},
		{"space", " 1.00", money.ErrInvalidAmount},
		{"plus sign", "+1.00", money.ErrInvalidAmount},
		{"negative", "-1.00", money.ErrNegativeAmount},
		{"negative zero", "-0.00", money.ErrNegativeAmount},
		{"nan", "NaN", money.ErrInvalidAmount},
		{"infinity", "Infinity", money.ErrInvalidAmount},
		{"exponent", "1e5", money.ErrInvalidAmount},
		{"three decimals", "1.005", money.ErrInvalidAmount},
		{"trailing dot", "1.", money.ErrInvalidAmount},
		{"leading dot", ".5", money.ErrInvalidAmount},
		{"comma", "1,00", money.ErrInvalidAmount},
		{"hex", "0x10", money.ErrInvalidAmount},
		{"unicode digits", "١٠", money.ErrInvalidAmount},
		{"overflow major", "92233720368547758.08", money.ErrOverflow},
		{"overflow huge", "99999999999999999999", money.ErrOverflow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := money.Parse(tc.in, "BRL")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("error %v is not classified as validation", err)
			}
		})
	}
}

func TestParseRejectsInvalidCurrency(t *testing.T) {
	for _, code := range []string{"", "BR", "BRLL", "brl", "B1L", "R$ "} {
		if _, err := money.Parse("1.00", code); !errors.Is(err, money.ErrInvalidCurrency) {
			t.Errorf("currency %q: error = %v, want ErrInvalidCurrency", code, err)
		}
	}
}

func TestParseSignedAllowsNegativeForInternalUse(t *testing.T) {
	m, err := money.ParseSigned("-0.50", "BRL")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Minor() != -50 || m.Amount() != "-0.50" || !m.IsNegative() {
		t.Fatalf("got %s (%d)", m, m.Minor())
	}
}

func TestAmountFormatsBoundaries(t *testing.T) {
	cases := []struct {
		minor int64
		want  string
	}{
		{0, "0.00"},
		{5, "0.05"},
		{-5, "-0.05"},
		{123456, "1234.56"},
		{math.MaxInt64, "92233720368547758.07"},
		{math.MinInt64, "-92233720368547758.08"},
	}
	for _, tc := range cases {
		m, err := money.New(tc.minor, money.BRL)
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Amount(); got != tc.want {
			t.Errorf("Amount(%d) = %q, want %q", tc.minor, got, tc.want)
		}
	}
}

func TestJSONRoundTrip(t *testing.T) {
	m, _ := money.Parse("25.5", "BRL")
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"amount":"25.50","currency":"BRL"}` {
		t.Fatalf("marshal = %s", data)
	}

	var back money.Money
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Equal(m) {
		t.Fatalf("round trip = %s, want %s", back, m)
	}
}

func TestJSONUnmarshalRejectsNumbersAndNegatives(t *testing.T) {
	cases := map[string]error{
		`{"amount":25.00,"currency":"BRL"}`:   money.ErrInvalidAmount,
		`{"amount":"-1.00","currency":"BRL"}`: money.ErrNegativeAmount,
		`{"amount":"1.000","currency":"BRL"}`: money.ErrInvalidAmount,
		`{"amount":"1.00","currency":"brl"}`:  money.ErrInvalidCurrency,
		`{"amount":"1.00"}`:                   money.ErrInvalidCurrency,
		`{"currency":"BRL"}`:                  money.ErrInvalidAmount,
		`"25.00"`:                             money.ErrInvalidAmount,
	}
	for in, want := range cases {
		var m money.Money
		if err := json.Unmarshal([]byte(in), &m); !errors.Is(err, want) {
			t.Errorf("%s: error = %v, want %v", in, err, want)
		}
	}
}

func TestJSONMarshalRejectsZeroValue(t *testing.T) {
	if _, err := json.Marshal(money.Money{}); !errors.Is(err, money.ErrUninitialized) {
		t.Fatalf("error = %v, want ErrUninitialized", err)
	}
}
