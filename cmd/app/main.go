// Command app is the single binary of the wager service. The roles it runs
// (HTTP API, SQS consumer, outbox publisher) are selected by configuration.
package main

import (
	"context"
	"log/slog"
	"os"

	"go.uber.org/fx"
)

func main() {
	fx.New(
		fx.Provide(newLogger),
		fx.Invoke(registerLifecycleLog),
	).Run()
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil))
}

func registerLifecycleLog(lc fx.Lifecycle, log *slog.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			log.Info("wager service starting")
			return nil
		},
		OnStop: func(context.Context) error {
			log.Info("wager service stopped")
			return nil
		},
	})
}
