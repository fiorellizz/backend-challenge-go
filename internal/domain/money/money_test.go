package money_test

import (
	"errors"
	"math"
	"testing"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
)

func brl(t *testing.T, minor int64) money.Money {
	t.Helper()
	m, err := money.New(minor, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNewRejectsInvalidCurrency(t *testing.T) {
	if _, err := money.New(1, "xx"); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Fatalf("error = %v, want ErrInvalidCurrency", err)
	}
}

func TestZeroValueIsRejectedEverywhere(t *testing.T) {
	var zero money.Money
	valid := brl(t, 100)

	if err := zero.Validate(); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Validate: %v", err)
	}
	if _, err := zero.Add(valid); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Add: %v", err)
	}
	if _, err := valid.Sub(zero); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Sub: %v", err)
	}
	if _, err := zero.Neg(); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Neg: %v", err)
	}
	if _, err := zero.Compare(valid); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Compare: %v", err)
	}
}

func TestZeroPerCurrency(t *testing.T) {
	z, err := money.Zero(money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	if !z.IsZero() || z.IsPositive() || z.IsNegative() || z.Currency() != money.BRL {
		t.Fatalf("unexpected zero: %s", z)
	}
	if _, err := money.Zero("x"); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Fatalf("error = %v, want ErrInvalidCurrency", err)
	}
}

func TestArithmetic(t *testing.T) {
	a, b := brl(t, 10000), brl(t, 2500)

	sum, err := a.Add(b)
	if err != nil || sum.Minor() != 12500 {
		t.Fatalf("Add = %v, %v", sum, err)
	}
	diff, err := a.Sub(b)
	if err != nil || diff.Minor() != 7500 {
		t.Fatalf("Sub = %v, %v", diff, err)
	}
	neg, err := b.Neg()
	if err != nil || neg.Minor() != -2500 || !neg.IsNegative() {
		t.Fatalf("Neg = %v, %v", neg, err)
	}
	below, err := b.Sub(a)
	if err != nil || below.Minor() != -7500 {
		t.Fatalf("Sub below zero = %v, %v (negative differences are allowed)", below, err)
	}

	// Immutability: operands are untouched.
	if a.Minor() != 10000 || b.Minor() != 2500 {
		t.Fatalf("operands mutated: %s, %s", a, b)
	}
}

func TestArithmeticRejectsCurrencyMismatch(t *testing.T) {
	brl := brl(t, 100)
	usd, _ := money.New(100, "USD")

	if _, err := brl.Add(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Add: %v", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Sub: %v", err)
	}
	if _, err := brl.Compare(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Compare: %v", err)
	}
	if brl.Equal(usd) {
		t.Errorf("Equal must be false across currencies")
	}
	if _, err := brl.Add(usd); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("mismatch is not classified as validation: %v", err)
	}
}

func TestArithmeticOverflow(t *testing.T) {
	max, min, one := brl(t, math.MaxInt64), brl(t, math.MinInt64), brl(t, 1)
	minusOne := brl(t, -1)

	if _, err := max.Add(one); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("max + 1: %v", err)
	}
	if _, err := min.Add(minusOne); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("min + (-1): %v", err)
	}
	if _, err := min.Sub(one); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("min - 1: %v", err)
	}
	if _, err := max.Sub(minusOne); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("max - (-1): %v", err)
	}
	if _, err := min.Neg(); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("-min: %v", err)
	}
	if r, err := max.Neg(); err != nil || r.Minor() != -math.MaxInt64 {
		t.Errorf("-max = %v, %v", r, err)
	}
	if r, err := max.Add(brl(t, 0)); err != nil || r.Minor() != math.MaxInt64 {
		t.Errorf("max + 0 = %v, %v", r, err)
	}
}

func TestCompareAndEqual(t *testing.T) {
	small, big, alsoSmall := brl(t, 1), brl(t, 2), brl(t, 1)

	if c, _ := small.Compare(big); c != -1 {
		t.Errorf("small vs big = %d", c)
	}
	if c, _ := big.Compare(small); c != 1 {
		t.Errorf("big vs small = %d", c)
	}
	if c, _ := small.Compare(alsoSmall); c != 0 {
		t.Errorf("equal compare = %d", c)
	}
	if !small.Equal(alsoSmall) || small.Equal(big) {
		t.Errorf("Equal misbehaves")
	}
}

func TestStringIncludesCurrency(t *testing.T) {
	if s := brl(t, 2550).String(); s != "25.50 BRL" {
		t.Fatalf("String = %q", s)
	}
}
