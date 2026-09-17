package wagering

import (
	"errors"
	"fmt"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
)

// Effect is the balance movement a transaction produces.
type Effect struct {
	Direction wallet.Direction
	Amount    money.Money
}

// Effect resolves the movement for this transaction. LOSS moves nothing and
// returns ok=false. Reversals need their reference, already located by the
// caller, and validate it here; any disagreement is a Rejection.
//
//	BET      → DEBIT amount
//	WIN      → CREDIT amount
//	LOSS     → no movement
//	REFUND   → CREDIT amount of a PROCESSED BET
//	ROLLBACK → opposite of the reference (BET → CREDIT, WIN/REFUND → DEBIT)
func (t *WagerTransaction) Effect(reference *WagerTransaction) (Effect, bool, error) {
	switch t.s.Kind {
	case Opening, Win:
		return Effect{Direction: wallet.Credit, Amount: t.s.Money}, true, nil
	case Bet:
		return Effect{Direction: wallet.Debit, Amount: t.s.Money}, true, nil
	case Loss:
		return Effect{}, false, nil
	case Refund:
		if err := t.checkReference(reference, Bet); err != nil {
			return Effect{}, false, err
		}
		return Effect{Direction: wallet.Credit, Amount: t.s.Money}, true, nil
	case Rollback:
		if err := t.checkReference(reference, Bet, Win, Refund); err != nil {
			return Effect{}, false, err
		}
		dir := wallet.Debit
		if reference.s.Kind == Bet {
			dir = wallet.Credit
		}
		return Effect{Direction: dir, Amount: t.s.Money}, true, nil
	}
	return Effect{}, false, fmt.Errorf("%w: kind %q", errs.ErrValidation, string(t.s.Kind))
}

// checkReference verifies that the reference can be undone by this
// transaction: it is PROCESSED, of an allowed kind, and agrees on provider,
// player, wallet, currency, round and amount.
func (t *WagerTransaction) checkReference(ref *WagerTransaction, allowed ...Kind) error {
	if ref == nil {
		return fmt.Errorf("%w: %s requires its reference to be resolved first", errs.ErrValidation, t.s.Kind)
	}
	if ref.s.ID == t.s.ID {
		return reject(ReferenceMismatch, "transaction cannot reference itself")
	}
	if ref.s.ProviderID != t.s.ProviderID || ref.s.ExternalTransactionID != t.s.ReferenceExternalTransactionID {
		return reject(ReferenceMismatch, "reference %s does not belong to provider %s", t.s.ReferenceExternalTransactionID, t.s.ProviderID)
	}
	if ref.s.Status != Processed {
		return reject(ReferenceNotProcessed, "reference %s is %s", ref.s.ExternalTransactionID, ref.s.Status)
	}
	kindOK := false
	for _, k := range allowed {
		kindOK = kindOK || ref.s.Kind == k
	}
	if !kindOK {
		return reject(ReferenceMismatch, "%s cannot reverse a %s", t.s.Kind, ref.s.Kind)
	}
	switch {
	case ref.s.PlayerID != t.s.PlayerID:
		return reject(ReferenceMismatch, "reference belongs to another player")
	case ref.s.WalletID != t.s.WalletID:
		return reject(ReferenceMismatch, "reference belongs to another wallet")
	case ref.s.RoundID != t.s.RoundID:
		return reject(ReferenceMismatch, "reference belongs to round %s, not %s", ref.s.RoundID, t.s.RoundID)
	case !ref.s.Money.Equal(t.s.Money):
		return reject(ReferenceMismatch, "reference amount is %s, reversal is %s", ref.s.Money, t.s.Money)
	}
	return nil
}

// CheckWallet verifies the transaction targets this wallet consistently.
// Currency and player disagreements are business rejections; a wallet id
// disagreement is a programming error.
func (t *WagerTransaction) CheckWallet(w *wallet.Wallet) error {
	if w.ID() != t.s.WalletID {
		return fmt.Errorf("%w: transaction targets wallet %s, got %s", errs.ErrValidation, t.s.WalletID, w.ID())
	}
	if w.Currency() != t.s.Money.Currency() {
		return reject(CurrencyMismatch, "wallet is %s, operation is %s", w.Currency(), t.s.Money.Currency())
	}
	if w.PlayerID() != t.s.PlayerID {
		return reject(PlayerMismatch, "wallet belongs to another player")
	}
	return nil
}

// RejectionFor translates an error raised while applying the effect to the
// wallet into the business rejection the provider should see. Anything
// that is not a known business outcome passes through as ok=false.
func (t *WagerTransaction) RejectionFor(err error) (*Rejection, bool) {
	if r, ok := AsRejection(err); ok {
		return r, true
	}
	if errors.Is(err, wallet.ErrInsufficientBalance) {
		code := InsufficientBalance
		if t.s.Kind.IsReversal() {
			code = ReversalInsufficientBalance
		}
		return &Rejection{Code: code, Reason: err.Error()}, true
	}
	if errors.Is(err, money.ErrCurrencyMismatch) {
		return &Rejection{Code: CurrencyMismatch, Reason: err.Error()}, true
	}
	return nil, false
}
