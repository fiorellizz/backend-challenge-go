// Package postgres implements the persistence ports with pgx and explicit
// SQL. Every query lives in a constant next to the method that runs it, so
// the transactions, locks and constraints the design relies on can be read
// directly from the source.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

// querier is what repositories run SQL on: the pool (autocommit reads) or
// a transaction (everything a use case writes).
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NewPool builds the connection pool. No connection is opened here; Ping
// during the lifecycle start is what proves the database is reachable.
func NewPool(cfg config.Config) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.Database.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pc.MaxConns = cfg.Database.MaxConns
	pc.MaxConnLifetime = 30 * time.Minute
	pc.HealthCheckPeriod = 30 * time.Second
	pc.ConnConfig.ConnectTimeout = 5 * time.Second

	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

// WaitReady pings until the database answers or ctx expires. A database
// that is still starting is the normal case right after `compose up`.
func WaitReady(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	for attempt := 1; ; attempt++ {
		err := pool.Ping(ctx)
		if err == nil {
			log.Info("postgres reachable", "attempt", attempt)
			return nil
		}
		log.Warn("postgres not reachable yet", "attempt", attempt, "error", err.Error())
		select {
		case <-ctx.Done():
			return fmt.Errorf("postgres never became reachable: %w", err)
		case <-time.After(time.Second):
		}
	}
}

// mapError classifies pgx errors into the domain taxonomy so callers can
// use errors.Is without importing pgx.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.ErrNotFound
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", errs.ErrConflict, pgErr.ConstraintName)
		case "23514", "23503", "23502": // check, foreign key, not null
			return fmt.Errorf("%w: constraint %s: %s", errs.ErrConflict, pgErr.ConstraintName, pgErr.Message)
		case "40001", "40P01": // serialization_failure, deadlock_detected
			return fmt.Errorf("%w: %s", errs.ErrTransient, pgErr.Message)
		}
		if pgErr.Code[:2] == "08" || pgErr.Code[:2] == "57" || pgErr.Code[:2] == "53" {
			// connection, operator intervention, insufficient resources
			return fmt.Errorf("%w: %s", errs.ErrTransient, pgErr.Message)
		}
		return fmt.Errorf("postgres %s: %s", pgErr.Code, pgErr.Message)
	}
	// Anything else (broken pipe, refused connection, pool closed) is an
	// infrastructure problem the caller should retry later.
	return fmt.Errorf("%w: %v", errs.ErrTransient, err)
}
