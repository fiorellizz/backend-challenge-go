// Command app is the single binary of the wager service. The roles it runs
// (HTTP API, SQS consumer, outbox publisher) are selected by APP_ROLES.
package main

import (
	"fmt"
	"os"

	"go.uber.org/fx"

	"github.com/fiorellizz/backend-challenge-go/internal/fxmodules"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

func main() {
	// Configuration is the one thing built outside the container: the
	// container's own options (timeouts, logger) depend on it, and a bad
	// environment should fail before anything else starts.
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	// Run blocks until SIGINT/SIGTERM, then stops every lifecycle hook in
	// reverse order within cfg.ShutdownTimeout.
	fx.New(fxmodules.App(cfg)...).Run()
}
