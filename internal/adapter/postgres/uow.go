package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// UnitOfWork opens SQL transactions and hands the use case a set of
// repositories bound to that transaction.
type UnitOfWork struct {
	pool *pgxpool.Pool
}

// NewUnitOfWork binds the unit of work to the pool.
func NewUnitOfWork(pool *pgxpool.Pool) *UnitOfWork {
	return &UnitOfWork{pool: pool}
}

// NewRepositories returns repositories bound to the pool, for reads that
// need no transaction.
func NewRepositories(pool *pgxpool.Pool) usecase.Repositories {
	return repositories(pool)
}

func repositories(q querier) usecase.Repositories {
	return usecase.Repositories{
		Wallets:      &WalletRepository{q: q},
		Transactions: &TransactionRepository{q: q},
		Ledger:       &LedgerRepository{q: q},
		Inbox:        &InboxRepository{q: q},
		Outbox:       &OutboxRepository{q: q},
	}
}

// WithinTx runs fn in a READ COMMITTED transaction. Row locks taken with
// SELECT ... FOR UPDATE inside fn are held until commit or rollback.
func (u *UnitOfWork) WithinTx(ctx context.Context, fn func(ctx context.Context, r usecase.Repositories) error) error {
	return u.run(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn)
}

// WithinSnapshot runs fn in a read-only REPEATABLE READ transaction.
func (u *UnitOfWork) WithinSnapshot(ctx context.Context, fn func(ctx context.Context, r usecase.Repositories) error) error {
	return u.run(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

func (u *UnitOfWork) run(ctx context.Context, opts pgx.TxOptions, fn func(ctx context.Context, r usecase.Repositories) error) (err error) {
	tx, err := u.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin: %w", mapError(err))
	}
	defer func() {
		// Rollback after a successful commit is a no-op; after a failure
		// it releases the locks and is the only cleanup that matters.
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) && err == nil {
			err = fmt.Errorf("rollback: %w", mapError(rbErr))
		}
	}()

	if err = fn(ctx, repositories(tx)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", mapError(err))
	}
	return nil
}
