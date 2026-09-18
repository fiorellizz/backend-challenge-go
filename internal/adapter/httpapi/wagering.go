package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// WageringHandler serves the provider-facing operation endpoints.
type WageringHandler struct {
	wagering *usecase.WageringService
	log      *slog.Logger
}

// NewWageringHandler builds the handler.
func NewWageringHandler(wagering *usecase.WageringService, log *slog.Logger) *WageringHandler {
	return &WageringHandler{wagering: wagering, log: log.With("component", "httpapi")}
}

// Register mounts the wagering routes.
func (h *WageringHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /wagering/transactions", h.submit)
	mux.HandleFunc("GET /wagering/transactions/{transactionId}", h.getByID)
	mux.HandleFunc("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", h.getByProvider)
}

type submitRequest struct {
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	PlayerID                       string      `json:"playerId"`
	WalletID                       string      `json:"walletId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	Kind                           string      `json:"kind"`
	Money                          money.Money `json:"money"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
}

// submitResponse is the outcome contract. Status decides the HTTP code:
//
//	PROCESSED         → 200, balance is the one observed at processing time
//	PENDING_REFERENCE → 202, nextAttemptAt says when the worker retries
//	REJECTED          → 422, failureCode is stable and documented
//
// idempotentReplay is true when the persisted outcome of an earlier
// submission was returned instead of processing the request again.
type submitResponse struct {
	TransactionID    string       `json:"transactionId"`
	Status           string       `json:"status"`
	Balance          *money.Money `json:"balance,omitempty"`
	FailureCode      string       `json:"failureCode,omitempty"`
	NextAttemptAt    *time.Time   `json:"nextAttemptAt,omitempty"`
	IdempotentReplay bool         `json:"idempotentReplay"`
	Error            *errorDetail `json:"error,omitempty"`
}

type transactionResponse struct {
	TransactionID                  string       `json:"transactionId"`
	ProviderID                     string       `json:"providerId,omitempty"`
	ExternalTransactionID          string       `json:"externalTransactionId,omitempty"`
	WalletID                       string       `json:"walletId"`
	PlayerID                       string       `json:"playerId"`
	RoundID                        string       `json:"roundId,omitempty"`
	GameID                         string       `json:"gameId,omitempty"`
	Kind                           string       `json:"kind"`
	Money                          money.Money  `json:"money"`
	Status                         string       `json:"status"`
	FailureCode                    string       `json:"failureCode,omitempty"`
	Balance                        *money.Money `json:"balance,omitempty"`
	WalletVersion                  int64        `json:"walletVersion,omitempty"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         string       `json:"referenceTransactionId,omitempty"`
	ReferenceAttempts              int          `json:"referenceAttempts,omitempty"`
	NextReferenceAttemptAt         *time.Time   `json:"nextReferenceAttemptAt,omitempty"`
	CreatedAt                      time.Time    `json:"createdAt"`
	UpdatedAt                      time.Time    `json:"updatedAt"`
	ProcessedAt                    *time.Time   `json:"processedAt,omitempty"`
}

func (h *WageringHandler) submit(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, r, h.log, fmt.Errorf("%w: Idempotency-Key header is required", errs.ErrValidation))
		return
	}
	if len(key) > 256 {
		writeError(w, r, h.log, fmt.Errorf("%w: Idempotency-Key exceeds 256 characters", errs.ErrValidation))
		return
	}
	var req submitRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, h.log, err)
		return
	}

	res, err := h.wagering.Process(r.Context(), usecase.ProcessInput{
		IdempotencyKey: key, ProviderID: req.ProviderID, ExternalTransactionID: req.ExternalTransactionID,
		PlayerID: req.PlayerID, WalletID: req.WalletID, RoundID: req.RoundID, GameID: req.GameID,
		Kind: req.Kind, Money: req.Money, ReferenceExternalTransactionID: req.ReferenceExternalTransactionID,
		CorrelationID: correlationID(r),
	})
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	status, body := toSubmitResponse(res)
	writeJSON(w, status, body)
}

func toSubmitResponse(res usecase.ProcessResult) (int, submitResponse) {
	tx := res.Transaction
	body := submitResponse{TransactionID: tx.ID().String(), Status: string(tx.Status()), IdempotentReplay: res.IdempotentReplay}
	switch tx.Status() {
	case wagering.Processed:
		balance, _, _ := tx.Result()
		body.Balance = &balance
		return http.StatusOK, body
	case wagering.PendingReference:
		next := tx.NextReferenceAttemptAt()
		body.NextAttemptAt = &next
		return http.StatusAccepted, body
	default: // REJECTED or FAILED are definitive outcomes of a well-formed request.
		body.FailureCode = string(tx.FailureCode())
		body.Error = &errorDetail{Code: "REJECTED", Message: "operation rejected: " + body.FailureCode}
		return http.StatusUnprocessableEntity, body
	}
}

func (h *WageringHandler) getByID(w http.ResponseWriter, r *http.Request) {
	tx, err := h.wagering.GetTransaction(r.Context(), id.TransactionID(r.PathValue("transactionId")))
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransactionResponse(tx))
}

func (h *WageringHandler) getByProvider(w http.ResponseWriter, r *http.Request) {
	tx, err := h.wagering.GetProviderTransaction(r.Context(), r.PathValue("providerId"), r.PathValue("externalTransactionId"))
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransactionResponse(tx))
}

func toTransactionResponse(tx *wagering.WagerTransaction) transactionResponse {
	s := tx.Snapshot()
	res := transactionResponse{
		TransactionID: s.ID.String(), ProviderID: s.ProviderID, ExternalTransactionID: s.ExternalTransactionID,
		WalletID: s.WalletID.String(), PlayerID: s.PlayerID.String(), RoundID: s.RoundID, GameID: s.GameID,
		Kind: string(s.Kind), Money: s.Money, Status: string(s.Status), FailureCode: string(s.FailureCode),
		ReferenceExternalTransactionID: s.ReferenceExternalTransactionID, ReferenceTransactionID: s.ReferenceTransactionID.String(),
		ReferenceAttempts: s.ReferenceAttempts, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
	if balance, version, ok := tx.Result(); ok {
		res.Balance, res.WalletVersion = &balance, version
	}
	if !s.NextReferenceAttemptAt.IsZero() && s.Status == wagering.PendingReference {
		res.NextReferenceAttemptAt = &s.NextReferenceAttemptAt
	}
	if !s.ProcessedAt.IsZero() {
		res.ProcessedAt = &s.ProcessedAt
	}
	return res
}
