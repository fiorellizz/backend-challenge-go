package fxmodules

import (
	"go.uber.org/fx"

	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// UseCases provides the application services. They depend only on the
// ports, which the adapter modules bind to concrete implementations.
var UseCases = fx.Module("usecase",
	fx.Provide(
		func() usecase.Clock { return usecase.SystemClock },
		usecase.NewWalletService,
	),
)
