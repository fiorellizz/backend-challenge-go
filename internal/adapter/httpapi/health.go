package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// ReadinessCheck probes one dependency. Adapters contribute their checks
// through the Fx value group "readiness", so the health endpoint learns
// about PostgreSQL and SQS without importing either.
type ReadinessCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

const readinessTimeout = 3 * time.Second

// Health serves the public liveness and readiness endpoints.
type Health struct {
	checks []ReadinessCheck
}

// NewHealth collects the readiness checks contributed by adapters.
func NewHealth(checks []ReadinessCheck) *Health {
	return &Health{checks: checks}
}

// Register mounts the endpoints. They are public: no authentication.
func (h *Health) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /health/live", h.live)
	mux.HandleFunc("GET /health/ready", h.ready)
}

// live answers as long as the process serves HTTP.
func (h *Health) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "up"})
}

// ready runs every dependency check concurrently and fails with 503 if any
// of them does, naming the dependency so operators see what is down.
func (h *Health) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	results := make([]string, len(h.checks))
	var wg sync.WaitGroup
	for i, c := range h.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Check(ctx); err != nil {
				results[i] = err.Error()
				return
			}
			results[i] = "ok"
		}()
	}
	wg.Wait()

	status := http.StatusOK
	checks := make(map[string]string, len(h.checks))
	for i, c := range h.checks {
		checks[c.Name] = results[i]
		if results[i] != "ok" {
			status = http.StatusServiceUnavailable
		}
	}
	body := map[string]any{"status": "ready", "checks": checks}
	if status != http.StatusOK {
		body["status"] = "unavailable"
	}
	writeJSON(w, status, body)
}
