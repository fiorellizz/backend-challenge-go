package fxmodules

import (
	"time"

	"go.uber.org/fx"

	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/logging"
)

// startTimeout bounds the boot: binding the port and, later, opening the
// connection pool and reaching the IdP.
const startTimeout = 60 * time.Second

// App assembles the container options for a process with the given
// configuration. main and the Fx composition tests share it, so what is
// tested is exactly what runs.
//
// Module order matters for shutdown: Fx stops lifecycle hooks in the
// reverse order they were registered, so components listed later stop
// earlier. HTTP goes last to stop first.
func App(cfg config.Config) []fx.Option {
	return []fx.Option{
		fx.Supply(cfg),
		fx.WithLogger(logging.ForFx),
		fx.StartTimeout(startTimeout),
		fx.StopTimeout(cfg.ShutdownTimeout),
		Platform,
		Postgres,
		OIDC,
		UseCases,
		Workers,
		HTTP,
	}
}
