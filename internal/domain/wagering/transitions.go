package wagering

import (
	"errors"
	"fmt"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
)

// State machine:
//
//	PENDING ──────────────► PROCESSED
//	   │  └───────────────► REJECTED
//	   │  └───────────────► FAILED
//	   ▼
//	PENDING_REFERENCE ────► PROCESSED | REJECTED | FAILED
//	   └─(retry, attempts++)─┘
//
// Terminal states never transition again.

var (
	// ErrTerminalState is returned when a transition is attempted on a
	// PROCESSED, REJECTED or FAILED transaction.
	ErrTerminalState = fmt.Errorf("%w: transaction is terminal", errs.ErrConflict)

	// ErrInvalidTransition is returned for a transition the current status
	// does not allow.
	ErrInvalidTransition = fmt.Errorf("%w: invalid transition", errs.ErrConflict)

	// ErrReferenceExpired signals that the retry budget or TTL for a
	// pending reference ran out; the caller rejects with ReferenceNotFound.
	ErrReferenceExpired = errors.New("reference wait expired")
)

// ReferencePolicy bounds how long a reversal waits for its reference.
// Backoff grows as BaseBackoff * 2^attempt, and the wait ends at whichever
// comes first: MaxAttempts or TTL after the first wait.
type ReferencePolicy struct {
	BaseBackoff time.Duration
	MaxAttempts int
	TTL         time.Duration
}

// Validate rejects a policy that could never stop retrying.
func (p ReferencePolicy) Validate() error {
	if p.BaseBackoff <= 0 || p.MaxAttempts <= 0 || p.TTL <= 0 {
		return fmt.Errorf("%w: reference policy must have positive backoff, attempts and ttl", errs.ErrValidation)
	}
	return nil
}

func (p ReferencePolicy) backoffFor(attempt int) time.Duration {
	d := p.BaseBackoff
	for i := 0; i < attempt && d < p.TTL; i++ {
		d *= 2
	}
	if d > p.TTL {
		d = p.TTL
	}
	return d
}

// MarkProcessed records a successful outcome with the balance and wallet
// version the provider will see on replays.
func (t *WagerTransaction) MarkProcessed(balanceAfter money.Money, walletVersion int64, now time.Time) error {
	if err := t.ensureOpen(); err != nil {
		return err
	}
	if err := balanceAfter.Validate(); err != nil {
		return err
	}
	if balanceAfter.Currency() != t.s.Money.Currency() {
		return fmt.Errorf("%w: result in %s for a %s transaction", money.ErrCurrencyMismatch, balanceAfter.Currency(), t.s.Money.Currency())
	}
	if walletVersion < 1 {
		return fmt.Errorf("%w: wallet version must be >= 1", errs.ErrValidation)
	}
	t.s.Status = Processed
	t.s.BalanceAfter = balanceAfter
	t.s.WalletVersionAfter = walletVersion
	t.s.ProcessedAt = now
	t.s.UpdatedAt = now
	return nil
}

// MarkRejected records a definitive business rejection.
func (t *WagerTransaction) MarkRejected(code FailureCode, now time.Time) error {
	return t.finish(Rejected, code, now)
}

// MarkFailed records a permanent infrastructure failure for audit.
func (t *WagerTransaction) MarkFailed(code FailureCode, now time.Time) error {
	return t.finish(Failed, code, now)
}

func (t *WagerTransaction) finish(status Status, code FailureCode, now time.Time) error {
	if err := t.ensureOpen(); err != nil {
		return err
	}
	if code == "" {
		return fmt.Errorf("%w: %s requires a failure code", errs.ErrValidation, status)
	}
	t.s.Status = status
	t.s.FailureCode = code
	t.s.UpdatedAt = now
	return nil
}

// AwaitReference moves a PENDING reversal to PENDING_REFERENCE, scheduling
// the first retry and fixing the deadline.
func (t *WagerTransaction) AwaitReference(policy ReferencePolicy, now time.Time) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if t.s.Status != Pending {
		return fmt.Errorf("%w: AwaitReference from %s", t.transitionErr(), t.s.Status)
	}
	if !t.s.Kind.IsReversal() {
		return fmt.Errorf("%w: %s cannot wait for a reference", ErrInvalidTransition, t.s.Kind)
	}
	t.s.Status = PendingReference
	t.s.ReferenceAttempts = 0
	t.s.NextReferenceAttemptAt = now.Add(policy.backoffFor(0))
	t.s.ReferenceDeadlineAt = now.Add(policy.TTL)
	t.s.UpdatedAt = now
	return nil
}

// RetryReference records one failed attempt to resolve the reference and
// schedules the next with exponential backoff. It returns
// ErrReferenceExpired once the policy is exhausted; the transaction is left
// in PENDING_REFERENCE for the caller to reject.
func (t *WagerTransaction) RetryReference(policy ReferencePolicy, now time.Time) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if t.s.Status != PendingReference {
		return fmt.Errorf("%w: RetryReference from %s", t.transitionErr(), t.s.Status)
	}
	t.s.ReferenceAttempts++
	t.s.UpdatedAt = now
	if t.s.ReferenceAttempts >= policy.MaxAttempts || !now.Before(t.s.ReferenceDeadlineAt) {
		return ErrReferenceExpired
	}
	t.s.NextReferenceAttemptAt = now.Add(policy.backoffFor(t.s.ReferenceAttempts))
	return nil
}

// ResolveReference stores the internal id of the referenced transaction.
func (t *WagerTransaction) ResolveReference(ref id.TransactionID) error {
	if err := t.ensureOpen(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if !t.s.Kind.IsReversal() && t.s.Kind != Win {
		return fmt.Errorf("%w: %s has no reference", ErrInvalidTransition, t.s.Kind)
	}
	t.s.ReferenceTransactionID = ref
	return nil
}

func (t *WagerTransaction) ensureOpen() error {
	if t.s.Status.IsTerminal() {
		return fmt.Errorf("%w: %s is %s", ErrTerminalState, t.s.ID, t.s.Status)
	}
	return nil
}

func (t *WagerTransaction) transitionErr() error {
	if t.s.Status.IsTerminal() {
		return ErrTerminalState
	}
	return ErrInvalidTransition
}
