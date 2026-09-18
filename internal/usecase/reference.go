package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
)

// ResolveNextPendingReference retries one parked reversal whose backoff
// has elapsed. It is what the reference worker calls in a loop, on every
// instance: SKIP LOCKED keeps instances from picking the same row, and a
// crash before commit simply releases the claim.
//
// Outcomes, all committed atomically with the claim:
//   - reference now present → settled like a fresh operation (PROCESSED or
//     REJECTED with the usual codes);
//   - still absent, budget left → attempts+1 and a later next attempt;
//   - still absent, budget exhausted → REJECTED with REFERENCE_NOT_FOUND
//     and a WagerTransactionRejected event.
//
// found is false when nothing was due.
func (s *WageringService) ResolveNextPendingReference(ctx context.Context) (outcome ReferenceOutcome, found bool, err error) {
	now := s.now()
	err = s.uow.WithinTx(ctx, func(ctx context.Context, r Repositories) error {
		tx, ok, err := r.Transactions.ClaimNextPendingReference(ctx, now)
		if err != nil || !ok {
			return err
		}
		found = true

		w, err := r.Wallets.GetForUpdate(ctx, tx.WalletID())
		if err != nil {
			return err
		}
		_, err = r.Transactions.GetByProviderExternalID(ctx, tx.ProviderID(), tx.ReferenceExternalID())
		switch {
		case errors.Is(err, errs.ErrNotFound):
			if err := s.retryOrExpire(ctx, r, tx, w, now); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if err := s.settle(ctx, r, tx, w, now); err != nil {
				return err
			}
		}
		outcome = outcomeOf(tx)
		return nil
	})
	return outcome, found, err
}

// ReferenceOutcome labels what the worker did with one pending reversal.
type ReferenceOutcome string

const (
	ReferenceProcessed ReferenceOutcome = "processed"
	ReferenceRejected  ReferenceOutcome = "rejected"
	ReferenceRetried   ReferenceOutcome = "retried"
)

func outcomeOf(tx *wagering.WagerTransaction) ReferenceOutcome {
	switch tx.Status() {
	case wagering.Processed:
		return ReferenceProcessed
	case wagering.Rejected, wagering.Failed:
		return ReferenceRejected
	default:
		return ReferenceRetried
	}
}

func (s *WageringService) retryOrExpire(ctx context.Context, r Repositories, tx *wagering.WagerTransaction, w *wallet.Wallet, now time.Time) error {
	err := tx.RetryReference(s.policy, now)
	if errors.Is(err, wagering.ErrReferenceExpired) {
		rejection := &wagering.Rejection{Code: wagering.ReferenceNotFound, Reason: "reference " + tx.ReferenceExternalID() + " never arrived"}
		return s.finish(ctx, r, tx, nil, w, rejection, now)
	}
	if err != nil {
		return err
	}
	return r.Transactions.Update(ctx, tx)
}
