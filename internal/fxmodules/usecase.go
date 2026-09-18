package fxmodules

import (
	"go.uber.org/fx"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// UseCases provides the application services. They depend only on the
// ports, which the adapter modules bind to concrete implementations.
var UseCases = fx.Module("usecase",
	fx.Provide(
		func() usecase.Clock { return usecase.SystemClock },
		newReferencePolicy,
		usecase.NewWalletService,
		usecase.NewWageringService,
		newOutboxService,
	),
)

// newReferencePolicy turns the REFERENCE_* settings into the domain policy
// that bounds how long a reversal waits for its reference.
func newReferencePolicy(cfg config.Config) wagering.ReferencePolicy {
	return wagering.ReferencePolicy{
		BaseBackoff: cfg.Reference.BaseBackoff,
		MaxAttempts: cfg.Reference.MaxAttempts,
		TTL:         cfg.Reference.TTL,
	}
}
