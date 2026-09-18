package usecase

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/event"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
)

// WageringService processes provider operations. HTTP and SQS both call
// Process; the only difference is that SQS passes an InboxMark so the
// message is recorded as handled in the same commit.
type WageringService struct {
	uow    UnitOfWork
	reads  Repositories
	now    Clock
	policy wagering.ReferencePolicy
}

// NewWageringService wires the service to its ports.
func NewWageringService(uow UnitOfWork, reads Repositories, now Clock, policy wagering.ReferencePolicy) (*WageringService, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &WageringService{uow: uow, reads: reads, now: now, policy: policy}, nil
}

// ProcessInput is a provider operation as received, before domain parsing.
type ProcessInput struct {
	IdempotencyKey                 string
	ProviderID                     string
	ExternalTransactionID          string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           string
	Money                          money.Money
	ReferenceExternalTransactionID string
	CorrelationID                  string

	// Inbox is set for messages consumed from SQS. Nil for HTTP.
	Inbox *InboxMark
}

// InboxMark identifies the message being handled so its inbox row is
// written in the same transaction as the outcome.
type InboxMark struct {
	ConsumerName string
	MessageID    string
	PayloadHash  []byte
	ReceivedAt   time.Time
}

// ProcessResult is the persisted outcome. Transaction.Status tells whether
// it was PROCESSED, REJECTED (with FailureCode) or PENDING_REFERENCE.
type ProcessResult struct {
	Transaction      *wagering.WagerTransaction
	IdempotentReplay bool
}

// ErrPayloadMismatch is returned when an idempotency key (or inbox message
// id) is reused with different business fields.
var ErrPayloadMismatch = fmt.Errorf("%w: idempotency key reused with a different payload", errs.ErrConflict)

// ErrKeyMismatch is returned when the same (provider, externalTransactionId)
// is resubmitted under a different idempotency key.
var ErrKeyMismatch = fmt.Errorf("%w: operation already submitted with another idempotency key", errs.ErrConflict)

// Process applies one operation in a single SQL transaction:
//
//  1. INSERT the transaction ON CONFLICT DO NOTHING (durable idempotency);
//     on conflict, compare the payload hash and replay or refuse.
//  2. SELECT the wallet FOR UPDATE (per-wallet serialization).
//  3. Resolve the reference for reversals; park as PENDING_REFERENCE if absent.
//  4. Apply the domain rule; rejections become REJECTED with a failure code.
//  5. Write ledger, wallet, transaction state, outbox events and the inbox row.
//
// Nothing is published or acknowledged before this commits.
func (s *WageringService) Process(ctx context.Context, in ProcessInput) (ProcessResult, error) {
	tx, err := s.newTransaction(in)
	if err != nil {
		return ProcessResult{}, err
	}
	now := s.now()

	var result ProcessResult
	err = s.uow.WithinTx(ctx, func(ctx context.Context, r Repositories) error {
		if in.Inbox != nil {
			if replay, found, err := s.inboxReplay(ctx, r, in); err != nil || found {
				result = replay
				return err
			}
		}

		if _, err := r.Wallets.Get(ctx, tx.WalletID()); err != nil {
			return fmt.Errorf("wallet %s: %w", tx.WalletID(), err)
		}

		inserted, err := r.Transactions.Insert(ctx, tx)
		if err != nil {
			return err
		}
		if !inserted {
			existing, err := s.existing(ctx, r, tx)
			if err != nil {
				return err
			}
			result = ProcessResult{Transaction: existing, IdempotentReplay: true}
			return s.markInbox(ctx, r, in, now)
		}

		w, err := r.Wallets.GetForUpdate(ctx, tx.WalletID())
		if err != nil {
			return err
		}
		if err := s.settle(ctx, r, tx, w, now); err != nil {
			return err
		}
		result = ProcessResult{Transaction: tx}
		return s.markInbox(ctx, r, in, now)
	})
	if err != nil {
		return ProcessResult{}, err
	}
	return result, nil
}

// newTransaction parses the raw input into the domain entity. Every
// validation failure here is a 400: nothing has been persisted.
func (s *WageringService) newTransaction(in ProcessInput) (*wagering.WagerTransaction, error) {
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency key is required", errs.ErrValidation)
	}
	if in.ProviderID == "" || in.ExternalTransactionID == "" || in.RoundID == "" || in.GameID == "" {
		return nil, fmt.Errorf("%w: providerId, externalTransactionId, roundId and gameId are required", errs.ErrValidation)
	}
	playerID, err := id.ParsePlayerID(in.PlayerID)
	if err != nil {
		return nil, fmt.Errorf("playerId: %w", err)
	}
	walletID, err := id.ParseWalletID(in.WalletID)
	if err != nil {
		return nil, fmt.Errorf("walletId: %w", err)
	}
	kind := wagering.Kind(in.Kind)
	if err := kind.Validate(); err != nil {
		return nil, err
	}
	hash, err := s.payloadHash(in)
	if err != nil {
		return nil, err
	}
	if in.CorrelationID == "" {
		in.CorrelationID = newID()
	}
	return wagering.NewExternal(wagering.ExternalParams{
		ID: id.TransactionID(newID()), ProviderID: in.ProviderID, ExternalTransactionID: in.ExternalTransactionID,
		IdempotencyKey: in.IdempotencyKey, PayloadHash: hash, WalletID: walletID, PlayerID: playerID,
		RoundID: in.RoundID, GameID: in.GameID, Kind: kind, Money: in.Money,
		ReferenceExternalTransactionID: in.ReferenceExternalTransactionID, CorrelationID: in.CorrelationID,
	}, s.now())
}

func (s *WageringService) payloadHash(in ProcessInput) ([]byte, error) {
	return wagering.Payload{
		ProviderID: in.ProviderID, ExternalTransactionID: in.ExternalTransactionID,
		PlayerID: in.PlayerID, WalletID: in.WalletID, RoundID: in.RoundID, GameID: in.GameID,
		Kind: wagering.Kind(in.Kind), Money: in.Money, ReferenceExternalTransactionID: in.ReferenceExternalTransactionID,
	}.Hash()
}

// existing loads the transaction that blocked the insert and decides
// between replay and conflict.
func (s *WageringService) existing(ctx context.Context, r Repositories, tx *wagering.WagerTransaction) (*wagering.WagerTransaction, error) {
	existing, err := r.Transactions.GetByIdempotencyKey(ctx, tx.IdempotencyKey())
	if errors.Is(err, errs.ErrNotFound) {
		// The key is new but (provider, externalTransactionId) is not.
		return nil, ErrKeyMismatch
	}
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(existing.PayloadHash(), tx.PayloadHash()) {
		return nil, ErrPayloadMismatch
	}
	if existing.ProviderID() != tx.ProviderID() {
		return nil, ErrPayloadMismatch
	}
	return existing, nil
}

// inboxReplay handles a message the consumer already committed: a
// redelivery. The stored outcome is returned without touching the wallet.
func (s *WageringService) inboxReplay(ctx context.Context, r Repositories, in ProcessInput) (ProcessResult, bool, error) {
	msg, found, err := r.Inbox.Find(ctx, in.Inbox.ConsumerName, in.Inbox.MessageID)
	if err != nil || !found {
		return ProcessResult{}, false, err
	}
	if !bytes.Equal(msg.PayloadHash, in.Inbox.PayloadHash) {
		return ProcessResult{}, true, fmt.Errorf("%w: message %s redelivered with a different payload", errs.ErrValidation, in.Inbox.MessageID)
	}
	existing, err := r.Transactions.GetByIdempotencyKey(ctx, in.IdempotencyKey)
	if err != nil {
		return ProcessResult{}, true, err
	}
	return ProcessResult{Transaction: existing, IdempotentReplay: true}, true, nil
}

func (s *WageringService) markInbox(ctx context.Context, r Repositories, in ProcessInput, now time.Time) error {
	if in.Inbox == nil {
		return nil
	}
	return r.Inbox.Insert(ctx, InboxMessage{
		ConsumerName: in.Inbox.ConsumerName, MessageID: in.Inbox.MessageID, PayloadHash: in.Inbox.PayloadHash,
		ReceivedAt: in.Inbox.ReceivedAt, CompletedAt: now,
	})
}

// settle runs steps 3-5 for a transaction whose wallet row is locked. It
// is shared with the pending-reference worker. Business rejections are
// persisted as REJECTED and do not surface as errors; only infrastructure
// failures do, which roll the transaction back.
func (s *WageringService) settle(ctx context.Context, r Repositories, tx *wagering.WagerTransaction, w *wallet.Wallet, now time.Time) error {
	if err := tx.CheckWallet(w); err != nil {
		return s.finish(ctx, r, tx, nil, w, err, now)
	}

	reference, err := s.resolveReference(ctx, r, tx)
	if errors.Is(err, errs.ErrNotFound) {
		return s.await(ctx, r, tx, now)
	}
	if err != nil {
		return s.finish(ctx, r, tx, nil, w, err, now)
	}

	effect, moves, err := tx.Effect(reference)
	if err != nil {
		return s.finish(ctx, r, tx, nil, w, err, now)
	}
	if !moves {
		return s.finish(ctx, r, tx, nil, w, nil, now)
	}

	versionBefore := w.Version()
	movement := wallet.Movement{EntryID: id.LedgerEntryID(newID()), TransactionID: tx.ID(), Amount: effect.Amount, At: now}
	var entry wallet.LedgerEntry
	if effect.Direction == wallet.Debit {
		entry, err = w.Debit(movement)
	} else {
		entry, err = w.Credit(movement)
	}
	if err != nil {
		return s.finish(ctx, r, tx, nil, w, err, now)
	}
	if err := r.Ledger.Insert(ctx, entry); err != nil {
		return err
	}
	if err := r.Wallets.UpdateBalance(ctx, w, versionBefore); err != nil {
		return err
	}
	return s.finish(ctx, r, tx, &entry, w, nil, now)
}

// resolveReference finds the transaction a reversal (or an optional WIN
// reference) points to and checks it has not been reversed already.
func (s *WageringService) resolveReference(ctx context.Context, r Repositories, tx *wagering.WagerTransaction) (*wagering.WagerTransaction, error) {
	if tx.ReferenceExternalID() == "" {
		return nil, nil
	}
	reference, err := r.Transactions.GetByProviderExternalID(ctx, tx.ProviderID(), tx.ReferenceExternalID())
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) && !tx.Kind().IsReversal() {
			return nil, nil // a WIN may name a bet that never arrived; it is informational
		}
		return nil, err
	}
	if err := tx.ResolveReference(reference.ID()); err != nil {
		return nil, err
	}
	if tx.Kind().IsReversal() {
		reversed, err := r.Transactions.HasSuccessfulReversal(ctx, reference.ID())
		if err != nil {
			return nil, err
		}
		if reversed {
			return nil, &wagering.Rejection{Code: wagering.ReferenceAlreadyReversed, Reason: "reference " + tx.ReferenceExternalID() + " was already reversed"}
		}
	}
	return reference, nil
}

// await parks a reversal until its reference arrives.
func (s *WageringService) await(ctx context.Context, r Repositories, tx *wagering.WagerTransaction, now time.Time) error {
	if err := tx.AwaitReference(s.policy, now); err != nil {
		return err
	}
	if err := r.Transactions.Update(ctx, tx); err != nil {
		return err
	}
	ev, err := tx.PendingReferenceEvent(s.meta(tx))
	if err != nil {
		return err
	}
	return r.Outbox.Insert(ctx, ev)
}

// finish records the terminal outcome and its events. cause is nil for
// success; a Rejection (or a wallet error that maps to one) rejects; any
// other error is infrastructure and propagates.
func (s *WageringService) finish(ctx context.Context, r Repositories, tx *wagering.WagerTransaction, entry *wallet.LedgerEntry, w *wallet.Wallet, cause error, now time.Time) error {
	var events []event.Envelope
	if cause == nil {
		if err := tx.MarkProcessed(w.Balance(), w.Version(), now); err != nil {
			return err
		}
		ev, err := tx.ProcessedEvent(s.meta(tx))
		if err != nil {
			return err
		}
		events = append(events, ev)
		if entry != nil {
			changed, err := wagering.BalanceChangedEvent(s.meta(tx), *entry, w.Version())
			if err != nil {
				return err
			}
			events = append(events, changed)
		}
	} else {
		rejection, ok := tx.RejectionFor(cause)
		if !ok {
			return cause
		}
		if err := tx.MarkRejected(rejection.Code, now); err != nil {
			return err
		}
		ev, err := tx.RejectedEvent(s.meta(tx))
		if err != nil {
			return err
		}
		events = append(events, ev)
	}

	if err := r.Transactions.Update(ctx, tx); err != nil {
		return err
	}
	for _, ev := range events {
		if err := r.Outbox.Insert(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

func (s *WageringService) meta(tx *wagering.WagerTransaction) event.Meta {
	return event.Meta{EventID: newID(), CorrelationID: tx.CorrelationID(), CausationID: tx.ID().String(), OccurredAt: s.now()}
}
