// Package usecase orchestrates the domain. It declares the ports it needs
// (this file) and the application services that drive them (the other
// files). Adapters implement the ports; the container binds them.
package usecase

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/event"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
)

// Repositories groups the persistence ports bound to one connection: the
// pool for reads, or a single SQL transaction inside UnitOfWork.WithinTx.
type Repositories struct {
	Wallets      WalletRepository
	Transactions TransactionRepository
	Ledger       LedgerRepository
	Inbox        InboxRepository
	Outbox       OutboxRepository
}

// UnitOfWork delimits SQL transactions. Everything a use case writes for
// one operation happens inside a single WithinTx call, so wallet, ledger,
// transaction state, inbox and outbox commit or roll back together.
type UnitOfWork interface {
	// WithinTx runs fn in a READ COMMITTED transaction and commits when fn
	// returns nil. Any error rolls back and is returned unchanged.
	WithinTx(ctx context.Context, fn func(ctx context.Context, r Repositories) error) error

	// WithinSnapshot runs fn in a read-only REPEATABLE READ transaction, so
	// every query sees the same committed state.
	WithinSnapshot(ctx context.Context, fn func(ctx context.Context, r Repositories) error) error
}

// WalletRepository persists the aggregate root.
type WalletRepository interface {
	// Insert stores a new wallet; errs.ErrConflict when (player, currency)
	// already has one.
	Insert(ctx context.Context, w *wallet.Wallet) error
	// Get rehydrates a wallet; errs.ErrNotFound when absent.
	Get(ctx context.Context, walletID id.WalletID) (*wallet.Wallet, error)
	// GetForUpdate locks the wallet row for the rest of the transaction.
	// Writers of the same wallet queue here; other wallets are unaffected.
	GetForUpdate(ctx context.Context, walletID id.WalletID) (*wallet.Wallet, error)
	// UpdateBalance writes balance, version and updated_at, guarded by the
	// version read before the change; errs.ErrConflict on a lost update.
	UpdateBalance(ctx context.Context, w *wallet.Wallet, expectedVersion int64) error
}

// TransactionRepository persists wager transactions.
type TransactionRepository interface {
	// Insert stores a PENDING transaction. It returns false without error
	// when the idempotency key or (provider, external id) already exists,
	// leaving the caller to load the existing row.
	Insert(ctx context.Context, tx *wagering.WagerTransaction) (inserted bool, err error)
	// Update writes the mutable fields (status, result, reference, retries).
	Update(ctx context.Context, tx *wagering.WagerTransaction) error
	GetByID(ctx context.Context, txID id.TransactionID) (*wagering.WagerTransaction, error)
	GetByIdempotencyKey(ctx context.Context, key string) (*wagering.WagerTransaction, error)
	GetByProviderExternalID(ctx context.Context, providerID, externalID string) (*wagering.WagerTransaction, error)
	// HasSuccessfulReversal reports whether a PROCESSED REFUND or ROLLBACK
	// already points to the reference. Called under the wallet lock; the
	// partial unique index remains the last line of defence.
	HasSuccessfulReversal(ctx context.Context, referenceID id.TransactionID) (bool, error)
	// ClaimNextPendingReference locks one PENDING_REFERENCE transaction
	// whose retry is due, skipping rows other instances hold. found=false
	// when none is due.
	ClaimNextPendingReference(ctx context.Context, now time.Time) (tx *wagering.WagerTransaction, found bool, err error)
}

// LedgerRow is a ledger entry with its stable sequence, used as the
// pagination cursor.
type LedgerRow struct {
	Seq   int64
	Entry wallet.LedgerEntry
}

// LedgerRepository appends to and reads the immutable ledger.
type LedgerRepository interface {
	Insert(ctx context.Context, e wallet.LedgerEntry) error
	// List returns up to limit entries of the wallet with seq > afterSeq,
	// in ascending seq order.
	List(ctx context.Context, walletID id.WalletID, afterSeq int64, limit int) ([]LedgerRow, error)
	// Sum returns credits minus debits in minor units and the number of
	// entries, for reconciliation.
	Sum(ctx context.Context, walletID id.WalletID) (minor int64, entries int64, err error)
}

// InboxMessage records that a consumer durably handled a message.
type InboxMessage struct {
	ConsumerName string
	MessageID    string
	PayloadHash  []byte
	ReceivedAt   time.Time
	CompletedAt  time.Time
}

// InboxRepository deduplicates consumer input. A row exists only when the
// transaction that handled the message committed.
type InboxRepository interface {
	Find(ctx context.Context, consumerName, messageID string) (msg InboxMessage, found bool, err error)
	Insert(ctx context.Context, msg InboxMessage) error
}

// OutboxRecord is a stored event with its delivery bookkeeping. Data is
// kept raw: the snapshot taken at commit time is what gets published.
type OutboxRecord struct {
	Envelope event.Envelope
	Data     json.RawMessage
	Attempts int
}

// OutboxRepository stores events in the same transaction as the facts
// that produced them and hands them to publishers afterwards.
type OutboxRepository interface {
	Insert(ctx context.Context, env event.Envelope) error
	// ClaimPending locks up to limit unpublished events whose next attempt
	// is due, skipping rows other publishers hold.
	ClaimPending(ctx context.Context, limit int, now time.Time) ([]OutboxRecord, error)
	MarkPublished(ctx context.Context, eventID string, now time.Time) error
	// MarkFailed records one failed attempt and when to try again.
	MarkFailed(ctx context.Context, eventID string, attempts int, nextAttemptAt time.Time, lastError string) error
	// OldestPendingAge returns how long the oldest unpublished event has
	// been waiting, or zero when nothing is pending.
	OldestPendingAge(ctx context.Context, now time.Time) (time.Duration, error)
}
