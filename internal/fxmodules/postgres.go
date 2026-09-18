package fxmodules

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/httpapi"
	"github.com/fiorellizz/backend-challenge-go/internal/adapter/postgres"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// Postgres provides the pool and the persistence ports. The pool's
// lifecycle hook is appended inside its constructor, which runs before any
// component that depends on it: Fx therefore closes the pool after every
// such component has stopped.
var Postgres = fx.Module("postgres",
	fx.Provide(
		newPool,
		fx.Annotate(postgres.NewUnitOfWork, fx.As(new(usecase.UnitOfWork))),
		postgres.NewRepositories,
		fx.Annotate(newReadiness, fx.ResultTags(`group:"readiness"`)),
	),
)

func newPool(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := postgres.NewPool(cfg)
	if err != nil {
		return nil, err
	}
	log = log.With("component", "postgres")
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			return postgres.WaitReady(ctx, pool, log)
		},
		OnStop: func(context.Context) error {
			pool.Close()
			log.Info("postgres pool closed")
			return nil
		},
	})
	return pool, nil
}

func newReadiness(pool *pgxpool.Pool) httpapi.ReadinessCheck {
	return httpapi.ReadinessCheck{Name: "postgres", Check: pool.Ping}
}
