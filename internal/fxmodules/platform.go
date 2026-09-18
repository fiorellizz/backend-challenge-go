// Package fxmodules wires the application. It is the only package that
// knows the whole dependency graph: every other package exposes plain
// constructors and stays unaware of Fx.
package fxmodules

import (
	"log/slog"
	"os"

	"go.uber.org/fx"

	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/logging"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/metrics"
)

// Platform provides the logger and announces the configuration. The
// config itself is supplied by App because it is loaded before the
// container exists.
var Platform = fx.Module("platform",
	fx.Provide(newLogger, metrics.New),
	fx.Invoke(announce),
)

func newLogger(cfg config.Config) *slog.Logger {
	return logging.New(cfg, os.Stdout)
}

// announce runs first among invokes and is where a future config check
// that needs other components would go. Today it only logs the profile.
func announce(cfg config.Config, log *slog.Logger) {
	log.Info("configuration loaded",
		"roles", cfg.Roles.String(),
		"httpAddr", cfg.HTTPAddr,
		"shutdownTimeout", cfg.ShutdownTimeout.String(),
	)
}
