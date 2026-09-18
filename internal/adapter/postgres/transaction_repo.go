package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
)

// TransactionRepository implements usecase.TransactionRepository.
type TransactionRepository struct {
	q querier
}

const (
	txColumns = `
		id, origin, provider_id, external_transaction_id, idempotency_key, payload_hash,
		round_id, game_id, wallet_id, player_id, kind, amount_minor, currency,
		reference_external_transaction_id, reference_transaction_id,
		status, failure_code, balance_after_minor, wallet_version_after,
		reference_attempts, next_reference_attempt_at, reference_deadline_at,
		correlation_id, created_at, updated_at, processed_at`

	// ON CONFLICT DO NOTHING covers every unique index of the table: the
	// idempotency key, (provider, external id) and the single-opening rule.
	// A concurrent duplicate blocks here until the first insert commits,
	// then sees zero rows affected and takes the replay path.
	insertTx = `
		INSERT INTO wager_transactions (` + txColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
		        $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)
		ON CONFLICT DO NOTHING`

	updateTx = `
		UPDATE wager_transactions
		   SET status = $2, failure_code = $3, balance_after_minor = $4, wallet_version_after = $5,
		       reference_transaction_id = $6, reference_attempts = $7,
		       next_reference_attempt_at = $8, reference_deadline_at = $9,
		       updated_at = $10, processed_at = $11
		 WHERE id = $1`

	selectTxByID       = `SELECT ` + txColumns + ` FROM wager_transactions WHERE id = $1`
	selectTxByKey      = `SELECT ` + txColumns + ` FROM wager_transactions WHERE idempotency_key = $1`
	selectTxByExternal = `SELECT ` + txColumns + ` FROM wager_transactions WHERE provider_id = $1 AND external_transaction_id = $2`

	selectHasReversal = `
		SELECT EXISTS (
			SELECT 1 FROM wager_transactions
			 WHERE reference_transaction_id = $1
			   AND kind IN ('REFUND', 'ROLLBACK')
			   AND status = 'PROCESSED')`

	// SKIP LOCKED lets several instances drain the pending set without
	// blocking each other; a row locked by a crashed instance is released
	// by its transaction abort.
	claimPendingReference = `
		SELECT ` + txColumns + `
		  FROM wager_transactions
		 WHERE status = 'PENDING_REFERENCE' AND next_reference_attempt_at <= $1
		 ORDER BY next_reference_attempt_at
		 LIMIT 1
		 FOR UPDATE SKIP LOCKED`
)

func (r *TransactionRepository) Insert(ctx context.Context, tx *wagering.WagerTransaction) (bool, error) {
	s := tx.Snapshot()
	tag, err := r.q.Exec(ctx, insertTx,
		s.ID.String(), string(s.Origin),
		nullStr(s.ProviderID), nullStr(s.ExternalTransactionID), nullStr(s.IdempotencyKey), s.PayloadHash,
		nullStr(s.RoundID), nullStr(s.GameID), s.WalletID.String(), s.PlayerID.String(),
		string(s.Kind), s.Money.Minor(), s.Money.Currency().String(),
		nullStr(s.ReferenceExternalTransactionID), nullStr(s.ReferenceTransactionID.String()),
		string(s.Status), nullStr(string(s.FailureCode)), resultMinor(s), nullInt(s.WalletVersionAfter),
		s.ReferenceAttempts, nullTime(s.NextReferenceAttemptAt), nullTime(s.ReferenceDeadlineAt),
		nullStr(s.CorrelationID), s.CreatedAt.UTC(), s.UpdatedAt.UTC(), nullTime(s.ProcessedAt))
	if err != nil {
		return false, fmt.Errorf("insert transaction: %w", mapError(err))
	}
	return tag.RowsAffected() == 1, nil
}

func (r *TransactionRepository) Update(ctx context.Context, tx *wagering.WagerTransaction) error {
	s := tx.Snapshot()
	tag, err := r.q.Exec(ctx, updateTx,
		s.ID.String(), string(s.Status), nullStr(string(s.FailureCode)), resultMinor(s), nullInt(s.WalletVersionAfter),
		nullStr(s.ReferenceTransactionID.String()), s.ReferenceAttempts,
		nullTime(s.NextReferenceAttemptAt), nullTime(s.ReferenceDeadlineAt),
		s.UpdatedAt.UTC(), nullTime(s.ProcessedAt))
	if err != nil {
		return fmt.Errorf("update transaction: %w", mapError(err))
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("update transaction %s: %w", s.ID, errs.ErrNotFound)
	}
	return nil
}

func (r *TransactionRepository) GetByID(ctx context.Context, txID id.TransactionID) (*wagering.WagerTransaction, error) {
	return scanTx(r.q.QueryRow(ctx, selectTxByID, txID.String()))
}

func (r *TransactionRepository) GetByIdempotencyKey(ctx context.Context, key string) (*wagering.WagerTransaction, error) {
	return scanTx(r.q.QueryRow(ctx, selectTxByKey, key))
}

func (r *TransactionRepository) GetByProviderExternalID(ctx context.Context, providerID, externalID string) (*wagering.WagerTransaction, error) {
	return scanTx(r.q.QueryRow(ctx, selectTxByExternal, providerID, externalID))
}

func (r *TransactionRepository) HasSuccessfulReversal(ctx context.Context, referenceID id.TransactionID) (bool, error) {
	var exists bool
	if err := r.q.QueryRow(ctx, selectHasReversal, referenceID.String()).Scan(&exists); err != nil {
		return false, fmt.Errorf("check reversal: %w", mapError(err))
	}
	return exists, nil
}

func (r *TransactionRepository) ClaimNextPendingReference(ctx context.Context, now time.Time) (*wagering.WagerTransaction, bool, error) {
	tx, err := scanTx(r.q.QueryRow(ctx, claimPendingReference, now.UTC()))
	if errors.Is(err, errs.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return tx, true, nil
}

func scanTx(row pgx.Row) (*wagering.WagerTransaction, error) {
	var (
		s                                                         wagering.Snapshot
		txID, origin, walletID, playerID, kind, currency, status  string
		providerID, externalID, key, roundID, gameID, refExternal *string
		refID, failureCode, correlationID                         *string
		amountMinor                                               int64
		balanceAfter, walletVersion                               *int64
		nextAttempt, deadline, processedAt                        *time.Time
	)
	err := row.Scan(
		&txID, &origin, &providerID, &externalID, &key, &s.PayloadHash,
		&roundID, &gameID, &walletID, &playerID, &kind, &amountMinor, &currency,
		&refExternal, &refID,
		&status, &failureCode, &balanceAfter, &walletVersion,
		&s.ReferenceAttempts, &nextAttempt, &deadline,
		&correlationID, &s.CreatedAt, &s.UpdatedAt, &processedAt)
	if err != nil {
		return nil, fmt.Errorf("load transaction: %w", mapError(err))
	}

	cur := money.Currency(strings.TrimSpace(currency))
	if s.Money, err = money.New(amountMinor, cur); err != nil {
		return nil, fmt.Errorf("load transaction %s: %w", txID, err)
	}
	if balanceAfter != nil {
		if s.BalanceAfter, err = money.New(*balanceAfter, cur); err != nil {
			return nil, fmt.Errorf("load transaction %s: %w", txID, err)
		}
	}
	s.ID, s.Origin = id.TransactionID(txID), wagering.Origin(origin)
	s.WalletID, s.PlayerID = id.WalletID(walletID), id.PlayerID(playerID)
	s.Kind, s.Status = wagering.Kind(kind), wagering.Status(status)
	s.ProviderID, s.ExternalTransactionID, s.IdempotencyKey = deref(providerID), deref(externalID), deref(key)
	s.RoundID, s.GameID = deref(roundID), deref(gameID)
	s.ReferenceExternalTransactionID = deref(refExternal)
	s.ReferenceTransactionID = id.TransactionID(deref(refID))
	s.FailureCode = wagering.FailureCode(deref(failureCode))
	s.CorrelationID = deref(correlationID)
	if walletVersion != nil {
		s.WalletVersionAfter = *walletVersion
	}
	s.CreatedAt, s.UpdatedAt = s.CreatedAt.UTC(), s.UpdatedAt.UTC()
	s.NextReferenceAttemptAt, s.ReferenceDeadlineAt, s.ProcessedAt = derefTime(nextAttempt), derefTime(deadline), derefTime(processedAt)
	return wagering.Rehydrate(s)
}

func resultMinor(s wagering.Snapshot) *int64 {
	if s.BalanceAfter.Currency() == "" {
		return nil
	}
	v := s.BalanceAfter.Minor()
	return &v
}
