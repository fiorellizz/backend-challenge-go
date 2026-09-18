package usecase

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/event"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
)

// WalletService opens, reads and reconciles wallets.
type WalletService struct {
	uow          UnitOfWork
	reads        Repositories
	now          Clock
	log          *slog.Logger
	onDivergence DivergenceObserver
}

// NewWalletService wires the service to its ports.
func NewWalletService(uow UnitOfWork, reads Repositories, now Clock, log *slog.Logger) *WalletService {
	return &WalletService{uow: uow, reads: reads, now: now, log: log.With("component", "wallet")}
}

// OpenWalletInput is the internal request to create a wallet.
type OpenWalletInput struct {
	PlayerID       id.PlayerID
	InitialBalance money.Money
	CorrelationID  string
}

// Open creates the wallet and, when the initial balance is positive, the
// OPENING transaction, its ledger entry and the two integration events, all
// in one commit. A zero initial balance creates only the wallet.
func (s *WalletService) Open(ctx context.Context, in OpenWalletInput) (*wallet.Wallet, error) {
	now := s.now()
	if in.CorrelationID == "" {
		in.CorrelationID = newID()
	}
	w, err := wallet.New(id.WalletID(newID()), in.PlayerID, in.InitialBalance.Currency(), now)
	if err != nil {
		return nil, err
	}
	if in.InitialBalance.IsNegative() {
		return nil, fmt.Errorf("%w: initial balance must not be negative", errs.ErrValidation)
	}

	// Everything below is computed in memory first; the unit of work only
	// persists an already consistent picture.
	var (
		opening *wagering.WagerTransaction
		entry   wallet.LedgerEntry
		events  []event.Envelope
	)
	if in.InitialBalance.IsPositive() {
		opening, err = wagering.NewOpening(wagering.OpeningParams{
			ID: id.TransactionID(newID()), WalletID: w.ID(), PlayerID: w.PlayerID(),
			Money: in.InitialBalance, CorrelationID: in.CorrelationID,
		}, now)
		if err != nil {
			return nil, err
		}
		entry, err = w.OpeningCredit(wallet.Movement{
			EntryID: id.LedgerEntryID(newID()), TransactionID: opening.ID(), Amount: in.InitialBalance, At: now,
		})
		if err != nil {
			return nil, err
		}
		if err = opening.MarkProcessed(entry.BalanceAfter(), w.Version(), now); err != nil {
			return nil, err
		}
		if events, err = s.openingEvents(opening, entry, w.Version()); err != nil {
			return nil, err
		}
	}

	err = s.uow.WithinTx(ctx, func(ctx context.Context, r Repositories) error {
		if err := r.Wallets.Insert(ctx, w); err != nil {
			return err
		}
		if opening == nil {
			return nil
		}
		if inserted, err := r.Transactions.Insert(ctx, opening); err != nil {
			return err
		} else if !inserted {
			return fmt.Errorf("%w: wallet %s already has an opening transaction", errs.ErrConflict, w.ID())
		}
		if err := r.Ledger.Insert(ctx, entry); err != nil {
			return err
		}
		for _, ev := range events {
			if err := r.Outbox.Insert(ctx, ev); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (s *WalletService) openingEvents(opening *wagering.WagerTransaction, entry wallet.LedgerEntry, version int64) ([]event.Envelope, error) {
	meta := event.Meta{
		EventID: newID(), CorrelationID: opening.CorrelationID(),
		CausationID: opening.ID().String(), OccurredAt: s.now(),
	}
	processed, err := opening.ProcessedEvent(meta)
	if err != nil {
		return nil, err
	}
	meta.EventID = newID()
	changed, err := wagering.BalanceChangedEvent(meta, entry, version)
	if err != nil {
		return nil, err
	}
	return []event.Envelope{processed, changed}, nil
}

// Get returns the wallet or errs.ErrNotFound.
func (s *WalletService) Get(ctx context.Context, walletID id.WalletID) (*wallet.Wallet, error) {
	if err := walletID.Validate(); err != nil {
		return nil, err
	}
	return s.reads.Wallets.Get(ctx, walletID)
}

// LedgerPage is one page of ledger entries plus the cursor for the next.
type LedgerPage struct {
	Entries    []wallet.LedgerEntry
	NextCursor string // empty when this is the last page
}

const (
	defaultLedgerLimit = 50
	maxLedgerLimit     = 200
)

// ListLedger pages through the wallet ledger in insertion order. The
// cursor is opaque to clients: it encodes the sequence of the last entry
// returned, which is stable because entries are appended under the wallet
// lock and never change.
func (s *WalletService) ListLedger(ctx context.Context, walletID id.WalletID, cursor string, limit int) (LedgerPage, error) {
	if err := walletID.Validate(); err != nil {
		return LedgerPage{}, err
	}
	afterSeq, err := decodeCursor(cursor)
	if err != nil {
		return LedgerPage{}, err
	}
	if limit <= 0 {
		limit = defaultLedgerLimit
	}
	if limit > maxLedgerLimit {
		limit = maxLedgerLimit
	}
	if _, err := s.reads.Wallets.Get(ctx, walletID); err != nil {
		return LedgerPage{}, err
	}

	// Ask for one extra row to know whether a next page exists.
	rows, err := s.reads.Ledger.List(ctx, walletID, afterSeq, limit+1)
	if err != nil {
		return LedgerPage{}, err
	}
	page := LedgerPage{Entries: make([]wallet.LedgerEntry, 0, len(rows))}
	if len(rows) > limit {
		rows = rows[:limit]
		page.NextCursor = encodeCursor(rows[len(rows)-1].Seq)
	}
	for _, r := range rows {
		page.Entries = append(page.Entries, r.Entry)
	}
	return page, nil
}

func encodeCursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(seq, 10)))
}

func decodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("%w: malformed cursor", errs.ErrValidation)
	}
	seq, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("%w: malformed cursor", errs.ErrValidation)
	}
	return seq, nil
}
