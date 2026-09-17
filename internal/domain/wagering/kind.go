// Package wagering models the operations providers submit against wallets:
// their state machine, the rules of each kind and how reversals relate to
// the transactions they undo.
package wagering

import (
	"errors"
	"fmt"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
)

// Kind is the operation type. OPENING is internal; the other five come from
// providers over HTTP or SQS.
type Kind string

const (
	Opening  Kind = "OPENING"
	Bet      Kind = "BET"
	Win      Kind = "WIN"
	Loss     Kind = "LOSS"
	Refund   Kind = "REFUND"
	Rollback Kind = "ROLLBACK"
)

// Validate rejects unknown kinds.
func (k Kind) Validate() error {
	switch k {
	case Opening, Bet, Win, Loss, Refund, Rollback:
		return nil
	}
	return fmt.Errorf("%w: kind %q", errs.ErrValidation, string(k))
}

// IsReversal reports whether the kind undoes a previous transaction.
func (k Kind) IsReversal() bool { return k == Refund || k == Rollback }

// IsExternal reports whether providers may submit the kind.
func (k Kind) IsExternal() bool { return k != Opening }

// Status is the processing state. PROCESSED, REJECTED and FAILED are
// terminal: a transaction in one of them never changes again.
type Status string

const (
	Pending          Status = "PENDING"
	PendingReference Status = "PENDING_REFERENCE"
	Processed        Status = "PROCESSED"
	Rejected         Status = "REJECTED"
	Failed           Status = "FAILED"
)

// Validate rejects unknown statuses.
func (s Status) Validate() error {
	switch s {
	case Pending, PendingReference, Processed, Rejected, Failed:
		return nil
	}
	return fmt.Errorf("%w: status %q", errs.ErrValidation, string(s))
}

// IsTerminal reports whether no further transition is allowed.
func (s Status) IsTerminal() bool {
	return s == Processed || s == Rejected || s == Failed
}

// Origin distinguishes the internal wallet opening from provider operations.
type Origin string

const (
	Internal Origin = "INTERNAL"
	External Origin = "EXTERNAL"
)

// FailureCode is the stable, documented reason attached to a REJECTED or
// FAILED transaction and returned to providers. Rejections are definitive:
// resubmitting the same operation yields the same code.
type FailureCode string

const (
	// InsufficientBalance: a BET exceeds the available balance.
	InsufficientBalance FailureCode = "INSUFFICIENT_BALANCE"
	// ReversalInsufficientBalance: a ROLLBACK that debits (undoing a WIN or
	// REFUND) exceeds the available balance. Distinct from a rejected bet so
	// operators can audit each case separately.
	ReversalInsufficientBalance FailureCode = "REVERSAL_INSUFFICIENT_BALANCE"
	// ReferenceNotFound: the referenced transaction never arrived before the
	// retry budget or TTL was exhausted.
	ReferenceNotFound FailureCode = "REFERENCE_NOT_FOUND"
	// ReferenceNotProcessed: the reference exists but is not PROCESSED
	// (still pending, rejected or failed), so there is nothing to undo.
	ReferenceNotProcessed FailureCode = "REFERENCE_NOT_PROCESSED"
	// ReferenceMismatch: the reference disagrees on provider, player, wallet,
	// currency, round or amount, or its kind cannot be reversed this way.
	ReferenceMismatch FailureCode = "REFERENCE_MISMATCH"
	// ReferenceAlreadyReversed: the reference already has one successful
	// reversal; a second one would return the same funds twice.
	ReferenceAlreadyReversed FailureCode = "REFERENCE_ALREADY_REVERSED"
	// CurrencyMismatch: the operation currency differs from the wallet's.
	CurrencyMismatch FailureCode = "CURRENCY_MISMATCH"
	// PlayerMismatch: the operation names a player who does not own the wallet.
	PlayerMismatch FailureCode = "PLAYER_MISMATCH"
	// PermanentFailure: infrastructure gave up on the operation. Recorded as
	// FAILED for audit; it is not a business decision.
	PermanentFailure FailureCode = "PERMANENT_FAILURE"
)

// Rejection is the error form of a business rejection. Callers detect it
// with errors.As and persist the code on the transaction.
type Rejection struct {
	Code   FailureCode
	Reason string
}

func (r *Rejection) Error() string {
	return fmt.Sprintf("rejected (%s): %s", r.Code, r.Reason)
}

// AsRejection extracts a Rejection from an error chain.
func AsRejection(err error) (*Rejection, bool) {
	var r *Rejection
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

func reject(code FailureCode, format string, args ...any) error {
	return &Rejection{Code: code, Reason: fmt.Sprintf(format, args...)}
}
