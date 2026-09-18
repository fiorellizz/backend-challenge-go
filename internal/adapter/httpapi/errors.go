package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/auth"
)

// Error contract. Every non-2xx response has this shape so clients can
// branch on `code` without parsing messages:
//
//	400 VALIDATION_ERROR   input can never be accepted as sent; fix and resend
//	401 UNAUTHENTICATED    missing, invalid or expired credentials
//	403 FORBIDDEN          valid credentials without permission for this resource
//	404 NOT_FOUND          the resource does not exist (or is not visible to you)
//	409 CONFLICT           request contradicts persisted state (reused key, duplicate wallet)
//	422 REJECTED           business rejection; body also carries the transaction outcome
//	503 UNAVAILABLE        a dependency is temporarily down; retry with backoff
//	500 INTERNAL_ERROR     unexpected failure, logged with the correlation id
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	status, code := classify(err)
	msg := err.Error()
	if status == http.StatusInternalServerError {
		log.ErrorContext(r.Context(), "request failed", "path", r.URL.Path, "error", err.Error())
		msg = "internal error"
	}
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg}})
}

func classify(err error) (int, string) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return http.StatusUnauthorized, "UNAUTHENTICATED"
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden, "FORBIDDEN"
	case errors.Is(err, errs.ErrValidation):
		return http.StatusBadRequest, "VALIDATION_ERROR"
	case errors.Is(err, errs.ErrNotFound):
		return http.StatusNotFound, "NOT_FOUND"
	case errors.Is(err, errs.ErrConflict):
		return http.StatusConflict, "CONFLICT"
	case errors.Is(err, errs.ErrTransient), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "UNAVAILABLE"
	}
	if _, ok := wagering.AsRejection(err); ok {
		return http.StatusUnprocessableEntity, "REJECTED"
	}
	return http.StatusInternalServerError, "INTERNAL_ERROR"
}
