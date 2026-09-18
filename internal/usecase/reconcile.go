package usecase

import (
	"context"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
)

// Reconciliation compares the stored balance with the one rebuilt from
// the ledger. Difference is stored minus calculated; a negative value
// means the ledger accounts for more money than the wallet holds.
type Reconciliation struct {
	WalletID          id.WalletID
	StoredBalance     money.Money
	CalculatedBalance money.Money
	Difference        money.Money
	Consistent        bool
	CheckedEntries    int64
}

// DivergenceObserver is notified when a reconciliation finds a mismatch,
// so the metrics layer can count it without the use case knowing about
// Prometheus. A nil observer is allowed.
type DivergenceObserver func(walletID id.WalletID, difference money.Money)

// Reconcile rebuilds the balance from every ledger entry, including the
// opening credit, inside one repeatable-read snapshot: the balance and the
// sum are read from the same committed state, so a concurrent operation
// cannot make a consistent wallet look divergent. Nothing is written.
func (s *WalletService) Reconcile(ctx context.Context, walletID id.WalletID) (Reconciliation, error) {
	if err := walletID.Validate(); err != nil {
		return Reconciliation{}, err
	}
	var out Reconciliation
	err := s.uow.WithinSnapshot(ctx, func(ctx context.Context, r Repositories) error {
		w, err := r.Wallets.Get(ctx, walletID)
		if err != nil {
			return err
		}
		minor, entries, err := r.Ledger.Sum(ctx, walletID)
		if err != nil {
			return err
		}
		calculated, err := money.New(minor, w.Currency())
		if err != nil {
			return err
		}
		difference, err := w.Balance().Sub(calculated)
		if err != nil {
			return err
		}
		out = Reconciliation{
			WalletID: walletID, StoredBalance: w.Balance(), CalculatedBalance: calculated,
			Difference: difference, Consistent: difference.IsZero(), CheckedEntries: entries,
		}
		return nil
	})
	if err != nil {
		return Reconciliation{}, err
	}
	if !out.Consistent {
		s.log.WarnContext(ctx, "reconciliation divergence",
			"walletId", walletID.String(), "storedBalance", out.StoredBalance.Amount(),
			"calculatedBalance", out.CalculatedBalance.Amount(), "difference", out.Difference.Amount(),
			"checkedEntries", out.CheckedEntries)
		if s.onDivergence != nil {
			s.onDivergence(walletID, out.Difference)
		}
	}
	return out, nil
}

// WithDivergenceObserver registers the callback used when a divergence is
// found. It returns the service for chaining in the container wiring.
func (s *WalletService) WithDivergenceObserver(fn DivergenceObserver) *WalletService {
	s.onDivergence = fn
	return s
}
