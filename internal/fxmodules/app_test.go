//go:build integration

package fxmodules_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/httpapi"
	"github.com/fiorellizz/backend-challenge-go/internal/fxmodules"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	env := map[string]string{
		"HTTP_ADDR":            "127.0.0.1:0",
		"LOG_LEVEL":            "error",
		"DATABASE_URL":         dbURL,
		"SQS_WAGER_QUEUE_URL":  "http://localhost:4566/000000000000/wager-transactions.fifo",
		"SQS_WAGER_DLQ_URL":    "http://localhost:4566/000000000000/wager-transactions-dlq.fifo",
		"SQS_EVENTS_QUEUE_URL": "http://localhost:4566/000000000000/wager-events.fifo",
		"OIDC_ISSUER_URL":      "http://localhost:8080/realms/wager",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestAppStartsServesHealthAndStops is the composition check: the exact
// options main uses are started, exercised and stopped under fxtest, which
// fails the test on any hook error or timeout.
func TestAppStartsServesHealthAndStops(t *testing.T) {
	var srv *httpapi.Server
	app := fxtest.New(t, append(fxmodules.App(testConfig(t)), fx.Populate(&srv))...)
	app.RequireStart()
	defer app.RequireStop()

	res, err := http.Get("http://" + srv.Addr() + "/health/live")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("live status = %d", res.StatusCode)
	}

	res, err = http.Get("http://" + srv.Addr() + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var ready struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(body, &ready); err != nil {
		t.Fatalf("ready body %s: %v", body, err)
	}
	// Each adapter contributes its check through the "readiness" group.
	if res.StatusCode != http.StatusOK || ready.Status != "ready" || ready.Checks["postgres"] != "ok" {
		t.Fatalf("ready = %d %s", res.StatusCode, body)
	}
}

func TestAppFailsToStartWhenPortIsBusy(t *testing.T) {
	var first *httpapi.Server
	holder := fxtest.New(t, append(fxmodules.App(testConfig(t)), fx.Populate(&first))...)
	holder.RequireStart()
	defer holder.RequireStop()

	cfg := testConfig(t)
	cfg.HTTPAddr = first.Addr()
	second := fxtest.New(t, fxmodules.App(cfg)...)
	if err := second.Start(t.Context()); err == nil {
		_ = second.Stop(t.Context())
		t.Fatal("second instance bound an already used port")
	}
}
