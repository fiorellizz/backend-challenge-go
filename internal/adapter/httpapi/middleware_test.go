package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/httpapi"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/metrics"
)

func TestInstrumentAddsCorrelationLogsAndMetrics(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	m := metrics.New()

	mux := httpapi.NewMux()
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	mux.Handle("GET /metrics", m.Handler())
	ts := httptest.NewServer(httpapi.Instrument(mux, m, log))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/things/42", nil)
	req.Header.Set("X-Correlation-ID", "corr-xyz")
	req.Header.Set("Authorization", "Bearer secret-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTeapot || res.Header.Get("X-Correlation-ID") != "corr-xyz" {
		t.Fatalf("status %d, correlation %q", res.StatusCode, res.Header.Get("X-Correlation-ID"))
	}

	res, _ = http.Get(ts.URL + "/things/43")
	res.Body.Close()
	if res.Header.Get("X-Correlation-ID") == "" {
		t.Fatalf("correlation id must be minted when absent")
	}

	var line map[string]any
	first := strings.SplitN(logs.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &line); err != nil {
		t.Fatalf("log line %q: %v", first, err)
	}
	if line["msg"] != "request" || line["route"] != "GET /things/{id}" || line["status"] != float64(418) || line["correlationId"] != "corr-xyz" {
		t.Fatalf("log line = %v", line)
	}
	if strings.Contains(logs.String(), "secret-token") {
		t.Fatalf("authorization header leaked into logs")
	}

	res, _ = http.Get(ts.URL + "/metrics")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, want := range []string{
		`http_requests_total{method="GET",route="GET /things/{id}",status="418"} 2`,
		`http_request_duration_seconds_count{method="GET",route="GET /things/{id}"} 2`,
		"outbox_lag_seconds", "reconciliation_divergences_total", "sqs_consumer_dlq_total", "outbox_published_total",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}
