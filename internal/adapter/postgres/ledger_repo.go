package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// LedgerRepository implements usecase.LedgerRepository. There is no update
// or delete: the table's triggers would refuse them anyway.
type LedgerRepository struct {
	q querier
}

const (
	insertLedgerEntry = `
		INSERT INTO wallet_ledger_entries
			(id, wallet_id, transaction_id, direction, amount_minor, currency,
			 balance_before_minor, balance_after_minor, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	// seq is assigned at insert under the wallet row lock, so ordering by
	// it is stable per wallet and safe to use as an opaque cursor.
	listLedger = `
		SELECT seq, id, wallet_id, transaction_id, direction, amount_minor, currency,
		       balance_before_minor, balance_after_minor, created_at
		  FROM wallet_ledger_entries
		 WHERE wallet_id = $1 AND seq > $2
		 ORDER BY seq
		 LIMIT $3`

	sumLedger = `
		SELECT COALESCE(SUM(CASE direction WHEN 'CREDIT' THEN amount_minor ELSE -amount_minor END), 0),
		       COUNT(*)
		  FROM wallet_ledger_entries
		 WHERE wallet_id = $1`
)

func (r *LedgerRepository) Insert(ctx context.Context, e wallet.LedgerEntry) error {
	_, err := r.q.Exec(ctx, insertLedgerEntry,
		e.ID().String(), e.WalletID().String(), e.TransactionID().String(), string(e.Direction()),
		e.Amount().Minor(), e.Amount().Currency().String(),
		e.BalanceBefore().Minor(), e.BalanceAfter().Minor(), e.CreatedAt().UTC())
	if err != nil {
		return fmt.Errorf("insert ledger entry: %w", mapError(err))
	}
	return nil
}

func (r *LedgerRepository) List(ctx context.Context, walletID id.WalletID, afterSeq int64, limit int) ([]usecase.LedgerRow, error) {
	rows, err := r.q.Query(ctx, listLedger, walletID.String(), afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list ledger: %w", mapError(err))
	}
	defer rows.Close()

	var out []usecase.LedgerRow
	for rows.Next() {
		var (
			seq, amount, before, after         int64
			entryID, wID, txID, direction, cur string
			createdAt                          time.Time
		)
		if err := rows.Scan(&seq, &entryID, &wID, &txID, &direction, &amount, &cur, &before, &after, &createdAt); err != nil {
			return nil, fmt.Errorf("scan ledger entry: %w", mapError(err))
		}
		c := money.Currency(strings.TrimSpace(cur))
		entry, err := wallet.NewLedgerEntry(wallet.LedgerEntryParams{
			ID: id.LedgerEntryID(entryID), WalletID: id.WalletID(wID), TransactionID: id.TransactionID(txID),
			Direction: wallet.Direction(direction), Amount: mustMoney(amount, c),
			BalanceBefore: mustMoney(before, c), BalanceAfter: mustMoney(after, c), CreatedAt: createdAt.UTC(),
		})
		if err != nil {
			return nil, fmt.Errorf("ledger entry %s: %w", entryID, err)
		}
		out = append(out, usecase.LedgerRow{Seq: seq, Entry: entry})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list ledger: %w", mapError(err))
	}
	return out, nil
}

func (r *LedgerRepository) Sum(ctx context.Context, walletID id.WalletID) (int64, int64, error) {
	var minor, entries int64
	if err := r.q.QueryRow(ctx, sumLedger, walletID.String()).Scan(&minor, &entries); err != nil {
		return 0, 0, fmt.Errorf("sum ledger: %w", mapError(err))
	}
	return minor, entries, nil
}

// mustMoney is for values read back from columns the schema already
// validates (CHAR(3) currency, BIGINT amount); an error here means a
// corrupted row, which NewLedgerEntry reports right after.
func mustMoney(minor int64, c money.Currency) money.Money {
	m, _ := money.New(minor, c)
	return m
}
