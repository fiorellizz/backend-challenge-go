package wagering

import (
	"fmt"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/event"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
)

// ProcessedEvent builds WagerTransactionProcessed from a PROCESSED transaction.
func (t *WagerTransaction) ProcessedEvent(m event.Meta) (event.Envelope, error) {
	if t.s.Status != Processed {
		return event.Envelope{}, fmt.Errorf("%w: processed event for a %s transaction", errs.ErrValidation, t.s.Status)
	}
	return event.NewWagerTransactionProcessed(m, event.WagerTransactionProcessed{
		TransactionID:         t.s.ID.String(),
		ProviderID:            t.s.ProviderID,
		ExternalTransactionID: t.s.ExternalTransactionID,
		WalletID:              t.s.WalletID.String(),
		PlayerID:              t.s.PlayerID.String(),
		RoundID:               t.s.RoundID,
		GameID:                t.s.GameID,
		Kind:                  string(t.s.Kind),
		Money:                 t.s.Money,
		Balance:               t.s.BalanceAfter,
		WalletVersion:         t.s.WalletVersionAfter,
		ProcessedAt:           t.s.ProcessedAt,
	})
}

// RejectedEvent builds WagerTransactionRejected from a REJECTED transaction.
func (t *WagerTransaction) RejectedEvent(m event.Meta) (event.Envelope, error) {
	if t.s.Status != Rejected {
		return event.Envelope{}, fmt.Errorf("%w: rejected event for a %s transaction", errs.ErrValidation, t.s.Status)
	}
	return event.NewWagerTransactionRejected(m, event.WagerTransactionRejected{
		TransactionID:         t.s.ID.String(),
		ProviderID:            t.s.ProviderID,
		ExternalTransactionID: t.s.ExternalTransactionID,
		WalletID:              t.s.WalletID.String(),
		PlayerID:              t.s.PlayerID.String(),
		Kind:                  string(t.s.Kind),
		Money:                 t.s.Money,
		FailureCode:           string(t.s.FailureCode),
		RejectedAt:            t.s.UpdatedAt,
	})
}

// PendingReferenceEvent builds WagerTransactionPendingReference from a
// PENDING_REFERENCE transaction.
func (t *WagerTransaction) PendingReferenceEvent(m event.Meta) (event.Envelope, error) {
	if t.s.Status != PendingReference {
		return event.Envelope{}, fmt.Errorf("%w: pending reference event for a %s transaction", errs.ErrValidation, t.s.Status)
	}
	return event.NewWagerTransactionPendingReference(m, event.WagerTransactionPendingReference{
		TransactionID:                  t.s.ID.String(),
		ProviderID:                     t.s.ProviderID,
		ExternalTransactionID:          t.s.ExternalTransactionID,
		WalletID:                       t.s.WalletID.String(),
		Kind:                           string(t.s.Kind),
		ReferenceExternalTransactionID: t.s.ReferenceExternalTransactionID,
		NextAttemptAt:                  t.s.NextReferenceAttemptAt,
		DeadlineAt:                     t.s.ReferenceDeadlineAt,
	})
}

// BalanceChangedEvent builds WalletBalanceChanged from the ledger entry a
// transaction produced and the wallet version after it.
func BalanceChangedEvent(m event.Meta, e wallet.LedgerEntry, walletVersion int64) (event.Envelope, error) {
	return event.NewWalletBalanceChanged(m, event.WalletBalanceChanged{
		WalletID:      e.WalletID().String(),
		TransactionID: e.TransactionID().String(),
		Direction:     string(e.Direction()),
		Money:         e.Amount(),
		BalanceBefore: e.BalanceBefore(),
		BalanceAfter:  e.BalanceAfter(),
		WalletVersion: walletVersion,
	})
}
