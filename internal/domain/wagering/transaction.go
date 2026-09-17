package wagering

import (
	"fmt"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
)

// Snapshot is the persistable state of a transaction. Optional fields use
// their zero value when absent; the shape rules in validateShape say which
// ones must be set for each origin.
type Snapshot struct {
	ID     id.TransactionID
	Origin Origin

	// External metadata (empty for INTERNAL).
	ProviderID            string
	ExternalTransactionID string
	IdempotencyKey        string
	PayloadHash           []byte
	RoundID               string
	GameID                string

	WalletID id.WalletID
	PlayerID id.PlayerID
	Kind     Kind
	Money    money.Money

	ReferenceExternalTransactionID string
	ReferenceTransactionID         id.TransactionID

	Status             Status
	FailureCode        FailureCode
	BalanceAfter       money.Money // set only when PROCESSED
	WalletVersionAfter int64       // set only when PROCESSED

	ReferenceAttempts      int
	NextReferenceAttemptAt time.Time
	ReferenceDeadlineAt    time.Time

	CorrelationID string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ProcessedAt   time.Time
}

// WagerTransaction is the entity. All state lives in the private snapshot
// and changes only through the validated transitions in transitions.go.
type WagerTransaction struct {
	s Snapshot
}

// ExternalParams is what a provider submits, already parsed into domain
// values, plus the identifiers the application layer generated.
type ExternalParams struct {
	ID                             id.TransactionID
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    []byte
	WalletID                       id.WalletID
	PlayerID                       id.PlayerID
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Money                          money.Money
	ReferenceExternalTransactionID string
	CorrelationID                  string
}

// NewExternal accepts a provider operation and starts it as PENDING. It
// enforces the input policy of each kind: OPENING is refused, LOSS must be
// zero, every other kind must be positive, reversals must name a reference
// and only WIN may optionally name one.
func NewExternal(p ExternalParams, now time.Time) (*WagerTransaction, error) {
	if p.Kind == Opening {
		return nil, fmt.Errorf("%w: kind OPENING is reserved for internal use", errs.ErrValidation)
	}
	if err := checkAmountPolicy(p.Kind, p.Money); err != nil {
		return nil, err
	}
	if p.Kind.IsReversal() && p.ReferenceExternalTransactionID == "" {
		return nil, fmt.Errorf("%w: %s requires referenceExternalTransactionId", errs.ErrValidation, p.Kind)
	}
	if p.ReferenceExternalTransactionID != "" && !p.Kind.IsReversal() && p.Kind != Win {
		return nil, fmt.Errorf("%w: %s does not accept a reference", errs.ErrValidation, p.Kind)
	}
	return Rehydrate(Snapshot{
		ID: p.ID, Origin: External,
		ProviderID: p.ProviderID, ExternalTransactionID: p.ExternalTransactionID,
		IdempotencyKey: p.IdempotencyKey, PayloadHash: p.PayloadHash,
		RoundID: p.RoundID, GameID: p.GameID,
		WalletID: p.WalletID, PlayerID: p.PlayerID, Kind: p.Kind, Money: p.Money,
		ReferenceExternalTransactionID: p.ReferenceExternalTransactionID,
		Status:                         Pending,
		CorrelationID:                  p.CorrelationID,
		CreatedAt:                      now,
		UpdatedAt:                      now,
	})
}

// OpeningParams describes the internal credit that opens a wallet.
type OpeningParams struct {
	ID            id.TransactionID
	WalletID      id.WalletID
	PlayerID      id.PlayerID
	Money         money.Money
	CorrelationID string
}

// NewOpening starts the internal OPENING transaction as PENDING. The amount
// must be positive: a zero initial balance creates no transaction at all.
func NewOpening(p OpeningParams, now time.Time) (*WagerTransaction, error) {
	if err := checkAmountPolicy(Opening, p.Money); err != nil {
		return nil, err
	}
	return Rehydrate(Snapshot{
		ID: p.ID, Origin: Internal, WalletID: p.WalletID, PlayerID: p.PlayerID,
		Kind: Opening, Money: p.Money, Status: Pending, CorrelationID: p.CorrelationID,
		CreatedAt: now, UpdatedAt: now,
	})
}

// Rehydrate rebuilds a transaction from persisted state, validating its
// shape without replaying any transition.
func Rehydrate(s Snapshot) (*WagerTransaction, error) {
	if err := validateShape(s); err != nil {
		return nil, err
	}
	return &WagerTransaction{s: s}, nil
}

func validateShape(s Snapshot) error {
	if err := s.ID.Validate(); err != nil {
		return err
	}
	if err := s.WalletID.Validate(); err != nil {
		return err
	}
	if err := s.PlayerID.Validate(); err != nil {
		return err
	}
	if err := s.Kind.Validate(); err != nil {
		return err
	}
	if err := s.Status.Validate(); err != nil {
		return err
	}
	if err := s.Money.Validate(); err != nil {
		return err
	}
	if s.Money.IsNegative() {
		return fmt.Errorf("%w: negative transaction amount %s", errs.ErrValidation, s.Money)
	}
	if s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: transaction without timestamps", errs.ErrValidation)
	}
	if s.Status == Rejected && s.FailureCode == "" {
		return fmt.Errorf("%w: rejected transaction without failure code", errs.ErrValidation)
	}
	if s.Status == Processed && (s.BalanceAfter.Currency() == "" || s.ProcessedAt.IsZero()) {
		return fmt.Errorf("%w: processed transaction without result", errs.ErrValidation)
	}
	if s.Status == PendingReference && !s.Kind.IsReversal() {
		return fmt.Errorf("%w: only reversals may wait for a reference", errs.ErrValidation)
	}

	switch s.Origin {
	case External:
		if s.Kind == Opening {
			return fmt.Errorf("%w: external transaction cannot be OPENING", errs.ErrValidation)
		}
		if s.ProviderID == "" || s.ExternalTransactionID == "" || s.IdempotencyKey == "" ||
			len(s.PayloadHash) == 0 || s.RoundID == "" || s.GameID == "" {
			return fmt.Errorf("%w: external transaction missing provider metadata", errs.ErrValidation)
		}
	case Internal:
		if s.Kind != Opening {
			return fmt.Errorf("%w: internal transaction must be OPENING", errs.ErrValidation)
		}
		if s.ProviderID != "" || s.ExternalTransactionID != "" || s.IdempotencyKey != "" ||
			len(s.PayloadHash) != 0 || s.RoundID != "" || s.GameID != "" ||
			s.ReferenceExternalTransactionID != "" || s.ReferenceTransactionID != "" {
			return fmt.Errorf("%w: internal transaction carries external metadata", errs.ErrValidation)
		}
	default:
		return fmt.Errorf("%w: origin %q", errs.ErrValidation, string(s.Origin))
	}
	return nil
}

// checkAmountPolicy applies the zero-value policy: LOSS is exactly zero,
// every other kind is strictly positive.
func checkAmountPolicy(kind Kind, m money.Money) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if kind == Loss {
		if !m.IsZero() {
			return fmt.Errorf("%w: LOSS amount must be 0.00, got %s", errs.ErrValidation, m)
		}
		return nil
	}
	if !m.IsPositive() {
		return fmt.Errorf("%w: %s amount must be greater than zero, got %s", errs.ErrValidation, kind, m)
	}
	return nil
}

// Snapshot returns a copy of the persistable state.
func (t *WagerTransaction) Snapshot() Snapshot { return t.s }

func (t *WagerTransaction) ID() id.TransactionID              { return t.s.ID }
func (t *WagerTransaction) Origin() Origin                    { return t.s.Origin }
func (t *WagerTransaction) ProviderID() string                { return t.s.ProviderID }
func (t *WagerTransaction) ExternalTransactionID() string     { return t.s.ExternalTransactionID }
func (t *WagerTransaction) IdempotencyKey() string            { return t.s.IdempotencyKey }
func (t *WagerTransaction) PayloadHash() []byte               { return t.s.PayloadHash }
func (t *WagerTransaction) WalletID() id.WalletID             { return t.s.WalletID }
func (t *WagerTransaction) PlayerID() id.PlayerID             { return t.s.PlayerID }
func (t *WagerTransaction) RoundID() string                   { return t.s.RoundID }
func (t *WagerTransaction) Kind() Kind                        { return t.s.Kind }
func (t *WagerTransaction) Money() money.Money                { return t.s.Money }
func (t *WagerTransaction) Status() Status                    { return t.s.Status }
func (t *WagerTransaction) FailureCode() FailureCode          { return t.s.FailureCode }
func (t *WagerTransaction) CorrelationID() string             { return t.s.CorrelationID }
func (t *WagerTransaction) ReferenceExternalID() string       { return t.s.ReferenceExternalTransactionID }
func (t *WagerTransaction) ReferenceID() id.TransactionID     { return t.s.ReferenceTransactionID }
func (t *WagerTransaction) ReferenceAttempts() int            { return t.s.ReferenceAttempts }
func (t *WagerTransaction) NextReferenceAttemptAt() time.Time { return t.s.NextReferenceAttemptAt }

// Result returns the balance and wallet version observed when the
// transaction was processed. ok is false unless the status is PROCESSED.
func (t *WagerTransaction) Result() (balance money.Money, walletVersion int64, ok bool) {
	if t.s.Status != Processed {
		return money.Money{}, 0, false
	}
	return t.s.BalanceAfter, t.s.WalletVersionAfter, true
}
