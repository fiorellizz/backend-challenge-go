package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/fiorellizz/backend-challenge-go/internal/platform/metrics"
)

type correlationKey struct{}

// Instrument wraps the router with the cross-cutting concerns of every
// request: a correlation id (taken from X-Correlation-ID or minted, echoed
// back in the response), one structured log line per request and the
// HTTP metrics. Nothing sensitive is logged: no headers, no bodies.
func Instrument(next http.Handler, m *metrics.Metrics, log *slog.Logger) http.Handler {
	log = log.With("component", "http")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		cid := r.Header.Get("X-Correlation-ID")
		if cid == "" || len(cid) > 128 {
			cid = uuid.Must(uuid.NewV7()).String()
		}
		w.Header().Set("X-Correlation-ID", cid)
		r = r.WithContext(context.WithValue(r.Context(), correlationKey{}, cid))

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		elapsed := time.Since(start)
		m.HTTPRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		m.HTTPRequestDuration.WithLabelValues(r.Method, route).Observe(elapsed.Seconds())

		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		log.Log(r.Context(), level, "request",
			"method", r.Method, "route", route, "path", r.URL.Path, "status", rec.status,
			"durationMs", elapsed.Milliseconds(), "correlationId", cid)
	})
}

// correlationID returns the id the middleware attached to the request.
func correlationID(r *http.Request) string {
	if v, ok := r.Context().Value(correlationKey{}).(string); ok && v != "" {
		return v
	}
	// Handlers mounted without the middleware (tests) still get an id.
	if v := r.Header.Get("X-Correlation-ID"); v != "" && len(v) <= 128 {
		return v
	}
	return uuid.Must(uuid.NewV7()).String()
}

// statusRecorder captures the status code for logs and metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
