package usecase

import (
	"context"
	"fmt"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
)

// GetTransaction returns a transaction by its internal id, whatever its
// state, so callers can follow pending work and read failure codes.
func (s *WageringService) GetTransaction(ctx context.Context, txID id.TransactionID) (*wagering.WagerTransaction, error) {
	if err := txID.Validate(); err != nil {
		return nil, err
	}
	return s.reads.Transactions.GetByID(ctx, txID)
}

// GetProviderTransaction returns a provider's transaction by its external
// id. The provider is part of the key, so one provider can never read
// another's transactions through this path.
func (s *WageringService) GetProviderTransaction(ctx context.Context, providerID, externalID string) (*wagering.WagerTransaction, error) {
	if providerID == "" || externalID == "" {
		return nil, fmt.Errorf("%w: providerId and externalTransactionId are required", errs.ErrValidation)
	}
	return s.reads.Transactions.GetByProviderExternalID(ctx, providerID, externalID)
}
