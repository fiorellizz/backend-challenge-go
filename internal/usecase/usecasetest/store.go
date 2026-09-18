// Package usecasetest provides an in-memory implementation of the
// persistence ports for unit tests of use cases and HTTP handlers. It
// mimics the properties the real adapter guarantees: unique constraints,
// version guards and transactional rollback on error.
package usecasetest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// Store holds the state. Fields are exported so tests can inspect and seed
// them directly.
type Store struct {
	mu           sync.Mutex
	Wallets      map[id.WalletID]wallet.Snapshot
	Transactions map[id.TransactionID]wagering.Snapshot
	Ledger       []usecase.LedgerRow
	Inbox        map[string]usecase.InboxMessage
	Outbox       []OutboxRow
	nextSeq      int64

	// FailOn makes the named repository method return FailErr, to test
	// that use cases roll back. Method names look like "Ledger.Insert".
	FailOn  string
	FailErr error
}

// OutboxRow is a stored event plus its publication bookkeeping.
type OutboxRow struct {
	Record      usecase.OutboxRecord
	Published   bool
	NextAttempt time.Time
	LastError   string
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		Wallets:      map[id.WalletID]wallet.Snapshot{},
		Transactions: map[id.TransactionID]wagering.Snapshot{},
		Inbox:        map[string]usecase.InboxMessage{},
	}
}

// Repos returns repositories bound to the store, for reads.
func (s *Store) Repos() usecase.Repositories {
	return usecase.Repositories{
		Wallets:      &walletRepo{s},
		Transactions: &txRepo{s},
		Ledger:       &ledgerRepo{s},
		Inbox:        &inboxRepo{s},
		Outbox:       &outboxRepo{s},
	}
}

// WithinTx serializes callers and restores the previous state when fn
// fails, which is what a rolled-back SQL transaction does.
func (s *Store) WithinTx(ctx context.Context, fn func(ctx context.Context, r usecase.Repositories) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	backup := s.snapshot()
	if err := fn(ctx, s.Repos()); err != nil {
		s.restore(backup)
		return err
	}
	return nil
}

// WithinSnapshot behaves like WithinTx; the in-memory store has no
// concurrent writers to isolate from.
func (s *Store) WithinSnapshot(ctx context.Context, fn func(ctx context.Context, r usecase.Repositories) error) error {
	return s.WithinTx(ctx, fn)
}

func (s *Store) snapshot() *Store {
	b := &Store{
		Wallets:      make(map[id.WalletID]wallet.Snapshot, len(s.Wallets)),
		Transactions: make(map[id.TransactionID]wagering.Snapshot, len(s.Transactions)),
		Inbox:        make(map[string]usecase.InboxMessage, len(s.Inbox)),
		Ledger:       append([]usecase.LedgerRow(nil), s.Ledger...),
		Outbox:       append([]OutboxRow(nil), s.Outbox...),
		nextSeq:      s.nextSeq,
	}
	for k, v := range s.Wallets {
		b.Wallets[k] = v
	}
	for k, v := range s.Transactions {
		b.Transactions[k] = v
	}
	for k, v := range s.Inbox {
		b.Inbox[k] = v
	}
	return b
}

func (s *Store) restore(b *Store) {
	s.Wallets, s.Transactions, s.Inbox = b.Wallets, b.Transactions, b.Inbox
	s.Ledger, s.Outbox, s.nextSeq = b.Ledger, b.Outbox, b.nextSeq
}

func (s *Store) fail(method string) error {
	if s.FailOn == method {
		return s.FailErr
	}
	return nil
}

// PendingOutbox returns the unpublished events, in insertion order.
func (s *Store) PendingOutbox() []usecase.OutboxRecord {
	var out []usecase.OutboxRecord
	for _, row := range s.Outbox {
		if !row.Published {
			out = append(out, row.Record)
		}
	}
	return out
}

// --- wallets ---------------------------------------------------------------

type walletRepo struct{ s *Store }

func (r *walletRepo) Insert(_ context.Context, w *wallet.Wallet) error {
	if err := r.s.fail("Wallets.Insert"); err != nil {
		return err
	}
	snap := w.Snapshot()
	if _, ok := r.s.Wallets[snap.ID]; ok {
		return fmt.Errorf("%w: wallets_pkey", errs.ErrConflict)
	}
	for _, existing := range r.s.Wallets {
		if existing.PlayerID == snap.PlayerID && existing.Currency == snap.Currency {
			return fmt.Errorf("%w: wallets_player_currency_unique", errs.ErrConflict)
		}
	}
	r.s.Wallets[snap.ID] = snap
	return nil
}

func (r *walletRepo) Get(_ context.Context, walletID id.WalletID) (*wallet.Wallet, error) {
	if err := r.s.fail("Wallets.Get"); err != nil {
		return nil, err
	}
	snap, ok := r.s.Wallets[walletID]
	if !ok {
		return nil, fmt.Errorf("wallet %s: %w", walletID, errs.ErrNotFound)
	}
	return wallet.Rehydrate(snap)
}

func (r *walletRepo) GetForUpdate(ctx context.Context, walletID id.WalletID) (*wallet.Wallet, error) {
	return r.Get(ctx, walletID)
}

func (r *walletRepo) UpdateBalance(_ context.Context, w *wallet.Wallet, expectedVersion int64) error {
	if err := r.s.fail("Wallets.UpdateBalance"); err != nil {
		return err
	}
	snap := w.Snapshot()
	current, ok := r.s.Wallets[snap.ID]
	if !ok || current.Version != expectedVersion {
		return fmt.Errorf("%w: wallet %s version", errs.ErrConflict, snap.ID)
	}
	r.s.Wallets[snap.ID] = snap
	return nil
}

// --- transactions ----------------------------------------------------------

type txRepo struct{ s *Store }

func (r *txRepo) Insert(_ context.Context, tx *wagering.WagerTransaction) (bool, error) {
	if err := r.s.fail("Transactions.Insert"); err != nil {
		return false, err
	}
	snap := tx.Snapshot()
	for _, existing := range r.s.Transactions {
		if snap.IdempotencyKey != "" && existing.IdempotencyKey == snap.IdempotencyKey {
			return false, nil
		}
		if snap.ProviderID != "" && existing.ProviderID == snap.ProviderID && existing.ExternalTransactionID == snap.ExternalTransactionID {
			return false, nil
		}
		if snap.Kind == wagering.Opening && existing.Kind == wagering.Opening && existing.WalletID == snap.WalletID {
			return false, nil
		}
	}
	r.s.Transactions[snap.ID] = snap
	return true, nil
}

func (r *txRepo) Update(_ context.Context, tx *wagering.WagerTransaction) error {
	if err := r.s.fail("Transactions.Update"); err != nil {
		return err
	}
	snap := tx.Snapshot()
	if _, ok := r.s.Transactions[snap.ID]; !ok {
		return fmt.Errorf("transaction %s: %w", snap.ID, errs.ErrNotFound)
	}
	r.s.Transactions[snap.ID] = snap
	return nil
}

func (r *txRepo) GetByID(_ context.Context, txID id.TransactionID) (*wagering.WagerTransaction, error) {
	if snap, ok := r.s.Transactions[txID]; ok {
		return wagering.Rehydrate(snap)
	}
	return nil, fmt.Errorf("transaction %s: %w", txID, errs.ErrNotFound)
}

func (r *txRepo) GetByIdempotencyKey(_ context.Context, key string) (*wagering.WagerTransaction, error) {
	return r.find(func(s wagering.Snapshot) bool { return s.IdempotencyKey == key })
}

func (r *txRepo) GetByProviderExternalID(_ context.Context, providerID, externalID string) (*wagering.WagerTransaction, error) {
	return r.find(func(s wagering.Snapshot) bool {
		return s.ProviderID == providerID && s.ExternalTransactionID == externalID
	})
}

func (r *txRepo) find(match func(wagering.Snapshot) bool) (*wagering.WagerTransaction, error) {
	for _, snap := range r.s.Transactions {
		if match(snap) {
			return wagering.Rehydrate(snap)
		}
	}
	return nil, errs.ErrNotFound
}

func (r *txRepo) HasSuccessfulReversal(_ context.Context, referenceID id.TransactionID) (bool, error) {
	for _, snap := range r.s.Transactions {
		if snap.ReferenceTransactionID == referenceID && snap.Kind.IsReversal() && snap.Status == wagering.Processed {
			return true, nil
		}
	}
	return false, nil
}

func (r *txRepo) ClaimNextPendingReference(_ context.Context, now time.Time) (*wagering.WagerTransaction, bool, error) {
	var best *wagering.Snapshot
	for _, snap := range r.s.Transactions {
		if snap.Status != wagering.PendingReference || snap.NextReferenceAttemptAt.After(now) {
			continue
		}
		if best == nil || snap.NextReferenceAttemptAt.Before(best.NextReferenceAttemptAt) {
			s := snap
			best = &s
		}
	}
	if best == nil {
		return nil, false, nil
	}
	tx, err := wagering.Rehydrate(*best)
	return tx, err == nil, err
}

// --- ledger ----------------------------------------------------------------

type ledgerRepo struct{ s *Store }

func (r *ledgerRepo) Insert(_ context.Context, e wallet.LedgerEntry) error {
	if err := r.s.fail("Ledger.Insert"); err != nil {
		return err
	}
	for _, row := range r.s.Ledger {
		if row.Entry.WalletID() == e.WalletID() && row.Entry.TransactionID() == e.TransactionID() {
			return fmt.Errorf("%w: ledger_wallet_transaction_unique", errs.ErrConflict)
		}
	}
	r.s.nextSeq++
	r.s.Ledger = append(r.s.Ledger, usecase.LedgerRow{Seq: r.s.nextSeq, Entry: e})
	return nil
}

func (r *ledgerRepo) List(_ context.Context, walletID id.WalletID, afterSeq int64, limit int) ([]usecase.LedgerRow, error) {
	var out []usecase.LedgerRow
	for _, row := range r.s.Ledger {
		if row.Entry.WalletID() == walletID && row.Seq > afterSeq && len(out) < limit {
			out = append(out, row)
		}
	}
	return out, nil
}

func (r *ledgerRepo) Sum(_ context.Context, walletID id.WalletID) (int64, int64, error) {
	var minor, count int64
	for _, row := range r.s.Ledger {
		if row.Entry.WalletID() != walletID {
			continue
		}
		count++
		if row.Entry.Direction() == wallet.Credit {
			minor += row.Entry.Amount().Minor()
		} else {
			minor -= row.Entry.Amount().Minor()
		}
	}
	return minor, count, nil
}

// --- inbox -----------------------------------------------------------------

type inboxRepo struct{ s *Store }

func inboxKey(consumer, messageID string) string { return consumer + "|" + messageID }

func (r *inboxRepo) Find(_ context.Context, consumerName, messageID string) (usecase.InboxMessage, bool, error) {
	msg, ok := r.s.Inbox[inboxKey(consumerName, messageID)]
	return msg, ok, nil
}

func (r *inboxRepo) Insert(_ context.Context, msg usecase.InboxMessage) error {
	if err := r.s.fail("Inbox.Insert"); err != nil {
		return err
	}
	key := inboxKey(msg.ConsumerName, msg.MessageID)
	if _, ok := r.s.Inbox[key]; ok {
		return fmt.Errorf("%w: inbox_consumer_message_unique", errs.ErrConflict)
	}
	r.s.Inbox[key] = msg
	return nil
}
