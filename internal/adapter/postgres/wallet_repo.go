package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
)

// WalletRepository implements usecase.WalletRepository.
type WalletRepository struct {
	q querier
}

const (
	walletColumns = `id, player_id, currency, balance_minor, version, created_at, updated_at`

	insertWallet = `
		INSERT INTO wallets (` + walletColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`

	selectWallet = `SELECT ` + walletColumns + ` FROM wallets WHERE id = $1`

	// FOR NO KEY UPDATE serializes writers of this wallet only; other
	// wallets' rows are untouched, so unrelated players proceed in parallel.
	//
	// NO KEY, not plain FOR UPDATE: inserting a wager_transactions row takes
	// a KEY SHARE lock on the referenced wallet (foreign key check). Two
	// operations on the same wallet would each hold KEY SHARE and then wait
	// for the other's to lift before acquiring FOR UPDATE: a deadlock. NO
	// KEY UPDATE is compatible with KEY SHARE, conflicts with itself and
	// with UPDATE, and is exactly the lock a balance change needs since the
	// primary key never changes.
	selectWalletForUpdate = selectWallet + ` FOR NO KEY UPDATE`

	// The version predicate makes a lost update impossible even if a caller
	// ever skipped the row lock: a stale version affects zero rows.
	updateWalletBalance = `
		UPDATE wallets
		   SET balance_minor = $2, version = $3, updated_at = $4
		 WHERE id = $1 AND version = $5`
)

func (r *WalletRepository) Insert(ctx context.Context, w *wallet.Wallet) error {
	s := w.Snapshot()
	_, err := r.q.Exec(ctx, insertWallet,
		s.ID.String(), s.PlayerID.String(), s.Currency.String(), s.Balance.Minor(),
		s.Version, s.CreatedAt.UTC(), s.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("insert wallet: %w", mapError(err))
	}
	return nil
}

func (r *WalletRepository) Get(ctx context.Context, walletID id.WalletID) (*wallet.Wallet, error) {
	return r.scan(r.q.QueryRow(ctx, selectWallet, walletID.String()))
}

func (r *WalletRepository) GetForUpdate(ctx context.Context, walletID id.WalletID) (*wallet.Wallet, error) {
	return r.scan(r.q.QueryRow(ctx, selectWalletForUpdate, walletID.String()))
}

func (r *WalletRepository) UpdateBalance(ctx context.Context, w *wallet.Wallet, expectedVersion int64) error {
	s := w.Snapshot()
	tag, err := r.q.Exec(ctx, updateWalletBalance,
		s.ID.String(), s.Balance.Minor(), s.Version, s.UpdatedAt.UTC(), expectedVersion)
	if err != nil {
		return fmt.Errorf("update wallet: %w", mapError(err))
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: wallet %s changed since version %d", errs.ErrConflict, s.ID, expectedVersion)
	}
	return nil
}

func (r *WalletRepository) scan(row pgx.Row) (*wallet.Wallet, error) {
	var (
		walletID, playerID, currency string
		balanceMinor, version        int64
		createdAt, updatedAt         time.Time
	)
	if err := row.Scan(&walletID, &playerID, &currency, &balanceMinor, &version, &createdAt, &updatedAt); err != nil {
		return nil, fmt.Errorf("load wallet: %w", mapError(err))
	}
	cur := money.Currency(strings.TrimSpace(currency))
	balance, err := money.New(balanceMinor, cur)
	if err != nil {
		return nil, fmt.Errorf("load wallet %s: %w", walletID, err)
	}
	return wallet.Rehydrate(wallet.Snapshot{
		ID: id.WalletID(walletID), PlayerID: id.PlayerID(playerID), Currency: cur,
		Balance: balance, Version: version, CreatedAt: createdAt.UTC(), UpdatedAt: updatedAt.UTC(),
	})
}
