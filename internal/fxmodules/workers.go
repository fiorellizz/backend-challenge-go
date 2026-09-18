package fxmodules

import (
	"context"
	"log/slog"
	"sync"

	"go.uber.org/fx"

	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
	"github.com/fiorellizz/backend-challenge-go/internal/worker"
)

// Workers registers the background loops according to the instance roles.
// The reference resolver belongs to the "consumer" role: it is the
// background half of processing, alongside the SQS consumer.
var Workers = fx.Module("workers",
	fx.Provide(newReferenceResolver),
	fx.Invoke(runReferenceResolver),
)

func newReferenceResolver(cfg config.Config, wagering *usecase.WageringService, log *slog.Logger) *worker.ReferenceResolver {
	return worker.NewReferenceResolver(wagering, cfg.Reference.PollInterval, log)
}

func runReferenceResolver(lc fx.Lifecycle, cfg config.Config, log *slog.Logger, r *worker.ReferenceResolver) {
	if !cfg.Roles.Has(config.RoleConsumer) {
		log.Info("consumer role disabled; reference resolver not started")
		return
	}
	runLoop(lc, "reference-resolver", r.Run, log)
}

// runLoop ties a Run(ctx) loop to the Fx lifecycle:
//
//   - OnStart launches the loop in a goroutine and returns immediately;
//   - OnStop cancels the loop's context and waits for it to return, but no
//     longer than the stop context allows. On timeout the error is
//     reported and the process exits anyway; any transaction the loop had
//     open is rolled back by the database, so nothing is half-applied.
func runLoop(lc fx.Lifecycle, name string, run func(context.Context), log *slog.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			wg.Add(1)
			go func() {
				defer wg.Done()
				run(ctx)
			}()
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()
			select {
			case <-done:
				log.Info("worker stopped", "worker", name)
				return nil
			case <-stopCtx.Done():
				log.Error("worker did not stop in time", "worker", name)
				return stopCtx.Err()
			}
		},
	})
}
