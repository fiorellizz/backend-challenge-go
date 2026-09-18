package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/metrics"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/httpapi"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	wageringdomain "github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/auth"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase/usecasetest"
)

const playerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"

type api struct {
	ts    *httptest.Server
	store *usecasetest.Store
	token string // bearer token sent by do(); "" sends none
}

func newAPI(t *testing.T) *api {
	t.Helper()
	store := usecasetest.NewStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := func() time.Time { return time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC) }
	wallets := usecase.NewWalletService(store, store.Repos(), clock, quietLog())
	wagering, err := usecase.NewWageringService(store, store.Repos(), clock, wageringdomain.ReferencePolicy{BaseBackoff: time.Second, MaxAttempts: 3, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{OIDC: config.OIDC{InternalRole: "internal-service", ProviderRole: "provider"}}
	guard := httpapi.NewAuth(fakeVerifier{}, cfg, log)
	mux := httpapi.NewMux()
	httpapi.NewWalletHandler(wallets, log).Register(mux, guard)
	httpapi.NewWageringHandler(wagering, metrics.New(), log).Register(mux, guard)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &api{ts: ts, store: store, token: tokenInternal}
}

// Tokens understood by fakeVerifier. Tests switch a.token to act as a
// different identity.
const (
	tokenInternal     = "internal-token"
	tokenProviderA    = "provider-a-token"
	tokenProviderB    = "provider-b-token"
	tokenNoRole       = "no-role-token"
	tokenProviderNoID = "provider-without-claim"
)

type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, raw string) (auth.Principal, error) {
	switch raw {
	case tokenInternal:
		return auth.Principal{Subject: "svc", ClientID: "wallet-internal", Roles: []string{"internal-service"}}, nil
	case tokenProviderA:
		return auth.Principal{Subject: "a", ClientID: "provider-a", Roles: []string{"provider"}, ProviderID: "provider-a"}, nil
	case tokenProviderB:
		return auth.Principal{Subject: "b", ClientID: "provider-b", Roles: []string{"provider"}, ProviderID: "provider-b"}, nil
	case tokenNoRole:
		return auth.Principal{Subject: "n", ClientID: "unauthorized-client"}, nil
	case tokenProviderNoID:
		return auth.Principal{Subject: "p", Roles: []string{"provider"}}, nil
	}
	return auth.Principal{}, auth.ErrUnauthenticated
}

func (a *api) do(t *testing.T, method, path, body string, headers ...string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, a.ts.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("non-JSON body (%d): %s", res.StatusCode, raw)
		}
	}
	return res.StatusCode, out
}

func (a *api) openWallet(t *testing.T, amount string) string {
	t.Helper()
	status, body := a.do(t, "POST", "/wallets", `{"playerId":"`+playerID+`","initialBalance":{"amount":"`+amount+`","currency":"BRL"}}`)
	if status != http.StatusCreated {
		t.Fatalf("open wallet: %d %v", status, body)
	}
	return body["id"].(string)
}

func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func TestOpenWalletReturnsCreatedContract(t *testing.T) {
	a := newAPI(t)
	status, body := a.do(t, "POST", "/wallets",
		`{"playerId":"`+playerID+`","initialBalance":{"amount":"1000.00","currency":"BRL"}}`,
		"X-Correlation-ID", "corr-42")
	if status != http.StatusCreated {
		t.Fatalf("status = %d %v", status, body)
	}
	balance := body["balance"].(map[string]any)
	if body["playerId"] != playerID || balance["amount"] != "1000.00" || balance["currency"] != "BRL" || body["version"] != float64(1) {
		t.Fatalf("body = %v", body)
	}
	if _, ok := body["id"].(string); !ok {
		t.Fatalf("missing id: %v", body)
	}
	for _, rec := range a.store.PendingOutbox() {
		if rec.Envelope.CorrelationID != "corr-42" {
			t.Fatalf("correlation id not propagated: %+v", rec.Envelope)
		}
	}
}

func TestOpenWalletConflictAndValidation(t *testing.T) {
	a := newAPI(t)
	a.openWallet(t, "1.00")

	status, body := a.do(t, "POST", "/wallets", `{"playerId":"`+playerID+`","initialBalance":{"amount":"1.00","currency":"BRL"}}`)
	if status != http.StatusConflict || errorCode(body) != "CONFLICT" {
		t.Errorf("duplicate wallet: %d %v", status, body)
	}

	cases := map[string]string{
		"bad player id":      `{"playerId":"nope","initialBalance":{"amount":"1.00","currency":"BRL"}}`,
		"negative balance":   `{"playerId":"` + playerID + `","initialBalance":{"amount":"-1.00","currency":"BRL"}}`,
		"float amount":       `{"playerId":"` + playerID + `","initialBalance":{"amount":1.00,"currency":"BRL"}}`,
		"three decimals":     `{"playerId":"` + playerID + `","initialBalance":{"amount":"1.005","currency":"BRL"}}`,
		"unknown field":      `{"playerId":"` + playerID + `","initialBalance":{"amount":"1.00","currency":"BRL"},"extra":1}`,
		"malformed json":     `{"playerId":`,
		"missing balance":    `{"playerId":"` + playerID + `"}`,
		"trailing garbage":   `{"playerId":"` + playerID + `","initialBalance":{"amount":"1.00","currency":"BRL"}} x`,
		"lowercase currency": `{"playerId":"` + playerID + `","initialBalance":{"amount":"1.00","currency":"brl"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			status, res := a.do(t, "POST", "/wallets", body)
			if status != http.StatusBadRequest || errorCode(res) != "VALIDATION_ERROR" {
				t.Fatalf("status = %d %v", status, res)
			}
		})
	}
	if len(a.store.Wallets) != 1 {
		t.Fatalf("invalid requests created wallets: %d", len(a.store.Wallets))
	}
}

func TestGetWallet(t *testing.T) {
	a := newAPI(t)
	walletID := a.openWallet(t, "25.50")

	status, body := a.do(t, "GET", "/wallets/"+walletID, "")
	if status != http.StatusOK || body["id"] != walletID || body["balance"].(map[string]any)["amount"] != "25.50" {
		t.Fatalf("get: %d %v", status, body)
	}
	status, body = a.do(t, "GET", "/wallets/0192f291-0000-7000-8000-000000000000", "")
	if status != http.StatusNotFound || errorCode(body) != "NOT_FOUND" {
		t.Errorf("missing: %d %v", status, body)
	}
	status, body = a.do(t, "GET", "/wallets/not-a-uuid", "")
	if status != http.StatusBadRequest {
		t.Errorf("bad id: %d %v", status, body)
	}
}

func TestLedgerPagination(t *testing.T) {
	a := newAPI(t)
	walletID := a.openWallet(t, "100.00")

	status, body := a.do(t, "GET", "/wallets/"+walletID+"/ledger?limit=50", "")
	if status != http.StatusOK {
		t.Fatalf("ledger: %d %v", status, body)
	}
	entries := body["entries"].([]any)
	if len(entries) != 1 || body["walletId"] != walletID {
		t.Fatalf("entries = %v", body)
	}
	first := entries[0].(map[string]any)
	if first["direction"] != "CREDIT" || first["balanceBefore"].(map[string]any)["amount"] != "0.00" || first["balanceAfter"].(map[string]any)["amount"] != "100.00" {
		t.Fatalf("first entry = %v", first)
	}
	if _, has := body["nextCursor"]; has {
		t.Fatalf("single page must omit nextCursor: %v", body)
	}

	status, body = a.do(t, "GET", "/wallets/"+walletID+"/ledger?limit=zero", "")
	if status != http.StatusBadRequest {
		t.Errorf("bad limit: %d %v", status, body)
	}
	status, body = a.do(t, "GET", "/wallets/"+walletID+"/ledger?cursor=%21%21", "")
	if status != http.StatusBadRequest {
		t.Errorf("bad cursor: %d %v", status, body)
	}
}

func TestTransientFailureMapsTo503(t *testing.T) {
	a := newAPI(t)
	a.store.FailOn, a.store.FailErr = "Wallets.Insert", errs.ErrTransient
	status, body := a.do(t, "POST", "/wallets", `{"playerId":"`+playerID+`","initialBalance":{"amount":"1.00","currency":"BRL"}}`)
	if status != http.StatusServiceUnavailable || errorCode(body) != "UNAVAILABLE" {
		t.Fatalf("status = %d %v", status, body)
	}
}

func TestErrorBodyShape(t *testing.T) {
	a := newAPI(t)
	status, body := a.do(t, "GET", "/wallets/0192f291-0000-7000-8000-000000000000", "")
	if status != http.StatusNotFound {
		t.Fatal(status)
	}
	e := body["error"].(map[string]any)
	if e["code"] != "NOT_FOUND" || !strings.Contains(e["message"].(string), "not found") {
		t.Fatalf("error = %v", e)
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestReconciliationEndpoint(t *testing.T) {
	a := newAPI(t)
	walletID := a.openWallet(t, "1000.00")
	a.submit(t, "k1", betBody(walletID, "tx-1", "BET", "25.00"))

	status, body := a.do(t, "POST", "/wallets/"+walletID+"/reconciliation", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d %v", status, body)
	}
	if body["walletId"] != walletID || body["consistent"] != true || body["checkedEntries"] != float64(2) ||
		body["storedBalance"].(map[string]any)["amount"] != "975.00" ||
		body["calculatedBalance"].(map[string]any)["amount"] != "975.00" ||
		body["difference"].(map[string]any)["amount"] != "0.00" {
		t.Fatalf("body = %v", body)
	}
	if status, _ := a.as(tokenProviderA).do(t, "POST", "/wallets/"+walletID+"/reconciliation", ""); status != http.StatusForbidden {
		t.Errorf("provider on reconciliation: %d", status)
	}
	if status, _ := a.do(t, "POST", "/wallets/0192f291-0000-7000-8000-000000000000/reconciliation", ""); status != http.StatusNotFound {
		t.Errorf("missing wallet: %d", status)
	}
}
