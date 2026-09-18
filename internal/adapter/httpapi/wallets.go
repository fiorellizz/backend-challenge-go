package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wallet"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

const maxBodyBytes = 64 << 10

// WalletHandler serves the internal wallet endpoints.
type WalletHandler struct {
	wallets *usecase.WalletService
	log     *slog.Logger
}

// NewWalletHandler builds the handler.
func NewWalletHandler(wallets *usecase.WalletService, log *slog.Logger) *WalletHandler {
	return &WalletHandler{wallets: wallets, log: log.With("component", "httpapi")}
}

// Register mounts the wallet routes. All of them are internal-only.
func (h *WalletHandler) Register(mux *http.ServeMux, guard *Auth) {
	mux.HandleFunc("POST /wallets", guard.Internal(h.open))
	mux.HandleFunc("GET /wallets/{walletId}", guard.Internal(h.get))
	mux.HandleFunc("GET /wallets/{walletId}/ledger", guard.Internal(h.ledger))
	mux.HandleFunc("POST /wallets/{walletId}/reconciliation", guard.Internal(h.reconcile))
}

type reconciliationResponse struct {
	WalletID          string      `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int64       `json:"checkedEntries"`
}

// reconcile rebuilds the balance from the ledger and reports the
// comparison. It changes nothing; a divergence is reported in the body,
// the logs and the reconciliation metric.
func (h *WalletHandler) reconcile(w http.ResponseWriter, r *http.Request) {
	rec, err := h.wallets.Reconcile(r.Context(), id.WalletID(r.PathValue("walletId")))
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	writeJSON(w, http.StatusOK, reconciliationResponse{
		WalletID: rec.WalletID.String(), StoredBalance: rec.StoredBalance, CalculatedBalance: rec.CalculatedBalance,
		Difference: rec.Difference, Consistent: rec.Consistent, CheckedEntries: rec.CheckedEntries,
	})
}

type openWalletRequest struct {
	PlayerID       string      `json:"playerId"`
	InitialBalance money.Money `json:"initialBalance"`
}

type walletResponse struct {
	ID        string      `json:"id"`
	PlayerID  string      `json:"playerId"`
	Balance   money.Money `json:"balance"`
	Version   int64       `json:"version"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

type ledgerEntryResponse struct {
	ID            string      `json:"id"`
	TransactionID string      `json:"transactionId"`
	Direction     string      `json:"direction"`
	Amount        money.Money `json:"amount"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	CreatedAt     time.Time   `json:"createdAt"`
}

type ledgerResponse struct {
	WalletID   string                `json:"walletId"`
	Entries    []ledgerEntryResponse `json:"entries"`
	NextCursor string                `json:"nextCursor,omitempty"`
}

func (h *WalletHandler) open(w http.ResponseWriter, r *http.Request) {
	var req openWalletRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, h.log, err)
		return
	}
	playerID, err := id.ParsePlayerID(req.PlayerID)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	created, err := h.wallets.Open(r.Context(), usecase.OpenWalletInput{
		PlayerID: playerID, InitialBalance: req.InitialBalance, CorrelationID: correlationID(r),
	})
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	writeJSON(w, http.StatusCreated, toWalletResponse(created))
}

func (h *WalletHandler) get(w http.ResponseWriter, r *http.Request) {
	found, err := h.wallets.Get(r.Context(), id.WalletID(r.PathValue("walletId")))
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	writeJSON(w, http.StatusOK, toWalletResponse(found))
}

func (h *WalletHandler) ledger(w http.ResponseWriter, r *http.Request) {
	walletID := id.WalletID(r.PathValue("walletId"))
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, r, h.log, fmt.Errorf("%w: limit must be a positive integer", errs.ErrValidation))
			return
		}
		limit = n
	}
	page, err := h.wallets.ListLedger(r.Context(), walletID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	res := ledgerResponse{WalletID: walletID.String(), Entries: make([]ledgerEntryResponse, 0, len(page.Entries)), NextCursor: page.NextCursor}
	for _, e := range page.Entries {
		res.Entries = append(res.Entries, ledgerEntryResponse{
			ID: e.ID().String(), TransactionID: e.TransactionID().String(), Direction: string(e.Direction()),
			Amount: e.Amount(), BalanceBefore: e.BalanceBefore(), BalanceAfter: e.BalanceAfter(), CreatedAt: e.CreatedAt(),
		})
	}
	writeJSON(w, http.StatusOK, res)
}

func toWalletResponse(w *wallet.Wallet) walletResponse {
	return walletResponse{
		ID: w.ID().String(), PlayerID: w.PlayerID().String(), Balance: w.Balance(),
		Version: w.Version(), CreatedAt: w.CreatedAt(), UpdatedAt: w.UpdatedAt(),
	}
}

// decodeJSON reads a bounded JSON body, refusing unknown fields and
// trailing data so malformed requests never reach the domain.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, errs.ErrValidation) {
			return err // already a domain validation error, e.g. from money.Money
		}
		return fmt.Errorf("%w: invalid JSON body: %v", errs.ErrValidation, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: unexpected data after JSON body", errs.ErrValidation)
	}
	return nil
}
