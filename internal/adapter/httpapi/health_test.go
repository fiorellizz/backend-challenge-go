package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/httpapi"
)

func serve(checks ...httpapi.ReadinessCheck) *httptest.Server {
	mux := httpapi.NewMux()
	httpapi.NewHealth(checks).Register(mux)
	return httptest.NewServer(mux)
}

func readyResponse(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	res, err := http.Get(url + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, body
}

func TestLiveAlwaysUp(t *testing.T) {
	ts := serve(httpapi.ReadinessCheck{Name: "db", Check: func(context.Context) error { return errors.New("down") }})
	defer ts.Close()

	res, err := http.Get(ts.URL + "/health/live")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("live = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
}

func TestReadyReportsEachDependency(t *testing.T) {
	ts := serve(
		httpapi.ReadinessCheck{Name: "postgres", Check: func(context.Context) error { return nil }},
		httpapi.ReadinessCheck{Name: "sqs", Check: func(context.Context) error { return errors.New("connection refused") }},
	)
	defer ts.Close()

	status, body := readyResponse(t, ts.URL)
	checks := body["checks"].(map[string]any)
	if status != http.StatusServiceUnavailable || body["status"] != "unavailable" {
		t.Fatalf("status = %d %v", status, body)
	}
	if checks["postgres"] != "ok" || checks["sqs"] != "connection refused" {
		t.Fatalf("checks = %v", checks)
	}
}

func TestReadyWhenAllChecksPass(t *testing.T) {
	ts := serve(httpapi.ReadinessCheck{Name: "postgres", Check: func(context.Context) error { return nil }})
	defer ts.Close()

	status, body := readyResponse(t, ts.URL)
	if status != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("status = %d %v", status, body)
	}
}

func TestReadyHonoursTimeout(t *testing.T) {
	ts := serve(httpapi.ReadinessCheck{Name: "slow", Check: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	defer ts.Close()

	status, body := readyResponse(t, ts.URL)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d %v", status, body)
	}
}
