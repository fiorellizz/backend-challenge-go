package wallet

import (
	"fmt"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
)

// Direction tells whether a ledger entry removes or adds funds.
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

// Validate rejects directions outside the two known values.
func (d Direction) Validate() error {
	if d != Debit && d != Credit {
		return fmt.Errorf("%w: direction %q", errs.ErrValidation, string(d))
	}
	return nil
}

// ErrLedgerArithmetic is returned when balanceAfter does not equal
// balanceBefore adjusted by the amount in the entry's direction.
var ErrLedgerArithmetic = fmt.Errorf("%w: ledger arithmetic", errs.ErrValidation)

// LedgerEntry is one immutable line of the append-only wallet ledger. It can
// only be built through NewLedgerEntry, which checks every invariant, so an
// entry that exists is an entry that is consistent. The same constructor
// serves creation and rehydration: an immutable value has no side effects
// to replay.
type LedgerEntry struct {
	id            id.LedgerEntryID
	walletID      id.WalletID
	transactionID id.TransactionID
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

// LedgerEntryParams carries the fields of a ledger entry.
type LedgerEntryParams struct {
	ID            id.LedgerEntryID
	WalletID      id.WalletID
	TransactionID id.TransactionID
	Direction     Direction
	Amount        money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	CreatedAt     time.Time
}

// NewLedgerEntry validates and builds an entry. The amount must be positive,
// all monies must share a currency, balances must be non-negative and
// BalanceAfter must equal BalanceBefore ± Amount according to Direction.
func NewLedgerEntry(p LedgerEntryParams) (LedgerEntry, error) {
	if err := p.ID.Validate(); err != nil {
		return LedgerEntry{}, err
	}
	if err := p.WalletID.Validate(); err != nil {
		return LedgerEntry{}, err
	}
	if err := p.TransactionID.Validate(); err != nil {
		return LedgerEntry{}, err
	}
	if err := p.Direction.Validate(); err != nil {
		return LedgerEntry{}, err
	}
	if p.CreatedAt.IsZero() {
		return LedgerEntry{}, fmt.Errorf("%w: ledger entry without creation time", errs.ErrValidation)
	}
	if err := p.Amount.Validate(); err != nil {
		return LedgerEntry{}, err
	}
	if !p.Amount.IsPositive() {
		return LedgerEntry{}, fmt.Errorf("%w: ledger amount must be positive, got %s", errs.ErrValidation, p.Amount)
	}
	if p.BalanceBefore.IsNegative() || p.BalanceAfter.IsNegative() {
		return LedgerEntry{}, fmt.Errorf("%w: ledger balances must be non-negative", errs.ErrValidation)
	}

	var expected money.Money
	var err error
	if p.Direction == Credit {
		expected, err = p.BalanceBefore.Add(p.Amount)
	} else {
		expected, err = p.BalanceBefore.Sub(p.Amount)
	}
	if err != nil {
		return LedgerEntry{}, err
	}
	if !expected.Equal(p.BalanceAfter) {
		return LedgerEntry{}, fmt.Errorf("%w: %s %s %s should give %s, got %s",
			ErrLedgerArithmetic, p.BalanceBefore, p.Direction, p.Amount, expected, p.BalanceAfter)
	}

	return LedgerEntry{
		id:            p.ID,
		walletID:      p.WalletID,
		transactionID: p.TransactionID,
		direction:     p.Direction,
		amount:        p.Amount,
		balanceBefore: p.BalanceBefore,
		balanceAfter:  p.BalanceAfter,
		createdAt:     p.CreatedAt,
	}, nil
}

func (e LedgerEntry) ID() id.LedgerEntryID            { return e.id }
func (e LedgerEntry) WalletID() id.WalletID           { return e.walletID }
func (e LedgerEntry) TransactionID() id.TransactionID { return e.transactionID }
func (e LedgerEntry) Direction() Direction            { return e.direction }
func (e LedgerEntry) Amount() money.Money             { return e.amount }
func (e LedgerEntry) BalanceBefore() money.Money      { return e.balanceBefore }
func (e LedgerEntry) BalanceAfter() money.Money       { return e.balanceAfter }
func (e LedgerEntry) CreatedAt() time.Time            { return e.createdAt }
