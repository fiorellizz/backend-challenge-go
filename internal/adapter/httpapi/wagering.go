package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/auth"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/metrics"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// WageringHandler serves the provider-facing operation endpoints.
type WageringHandler struct {
	wagering *usecase.WageringService
	guard    *Auth
	metrics  *metrics.Metrics
	log      *slog.Logger
}

// NewWageringHandler builds the handler.
func NewWageringHandler(wagering *usecase.WageringService, m *metrics.Metrics, log *slog.Logger) *WageringHandler {
	return &WageringHandler{wagering: wagering, metrics: m, log: log.With("component", "httpapi")}
}

// Register mounts the wagering routes. Submission and the provider query
// need a provider token bound to the providerId in the request; the query
// by internal id is open to both roles, with providers restricted to their
// own transactions.
func (h *WageringHandler) Register(mux *http.ServeMux, guard *Auth) {
	h.guard = guard
	mux.HandleFunc("POST /wagering/transactions", guard.Provider(h.submit))
	mux.HandleFunc("GET /wagering/transactions/{transactionId}", guard.Any(h.getByID))
	mux.HandleFunc("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", guard.Provider(h.getByProvider))
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
	// A provider may only submit operations under its own id; the token,
	// not the body, is the source of truth.
	if err := SameProvider(r, req.ProviderID); err != nil {
		writeError(w, r, h.log, err)
		return
	}

	start := time.Now()
	res, err := h.wagering.Process(r.Context(), usecase.ProcessInput{
		IdempotencyKey: key, ProviderID: req.ProviderID, ExternalTransactionID: req.ExternalTransactionID,
		PlayerID: req.PlayerID, WalletID: req.WalletID, RoundID: req.RoundID, GameID: req.GameID,
		Kind: req.Kind, Money: req.Money, ReferenceExternalTransactionID: req.ReferenceExternalTransactionID,
		CorrelationID: correlationID(r),
	})
	if err != nil {
		h.countConflict(err)
		writeError(w, r, h.log, err)
		return
	}
	tx := res.Transaction
	h.metrics.ObserveProcessing("http", string(tx.Kind()), string(tx.Status()), string(tx.FailureCode()), res.IdempotentReplay, time.Since(start))
	h.log.InfoContext(r.Context(), "operation handled",
		"correlationId", correlationID(r), "transactionId", tx.ID().String(), "walletId", tx.WalletID().String(),
		"providerId", tx.ProviderID(), "kind", string(tx.Kind()), "status", string(tx.Status()),
		"failureCode", string(tx.FailureCode()), "idempotentReplay", res.IdempotentReplay)
	status, body := toSubmitResponse(res)
	writeJSON(w, status, body)
}

func (h *WageringHandler) countConflict(err error) {
	switch {
	case errors.Is(err, usecase.ErrPayloadMismatch):
		h.metrics.ConflictsTotal.WithLabelValues("payload").Inc()
	case errors.Is(err, usecase.ErrKeyMismatch):
		h.metrics.ConflictsTotal.WithLabelValues("key").Inc()
	case errors.Is(err, errs.ErrConflict):
		h.metrics.ConflictsTotal.WithLabelValues("version").Inc()
	}
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
	// A provider sees only its own transactions; anything else looks
	// absent so the id space of other providers is not revealed.
	if p, _ := auth.FromContext(r.Context()); !h.guard.IsInternal(p) && tx.ProviderID() != p.ProviderID {
		writeError(w, r, h.log, fmt.Errorf("transaction: %w", errs.ErrNotFound))
		return
	}
	writeJSON(w, http.StatusOK, toTransactionResponse(tx))
}

func (h *WageringHandler) getByProvider(w http.ResponseWriter, r *http.Request) {
	if err := SameProvider(r, r.PathValue("providerId")); err != nil {
		writeError(w, r, h.log, err)
		return
	}
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
