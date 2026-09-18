package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestBusinessRoutesRequireAuthentication(t *testing.T) {
	a := newAPI(t)
	walletID := a.openWallet(t, "10.00")
	anon := a.as("")
	bad := a.as("garbage")

	routes := []struct{ method, path, body string }{
		{"POST", "/wallets", `{"playerId":"` + playerID + `","initialBalance":{"amount":"1.00","currency":"BRL"}}`},
		{"GET", "/wallets/" + walletID, ""},
		{"GET", "/wallets/" + walletID + "/ledger", ""},
		{"POST", "/wagering/transactions", betBody(walletID, "x", "BET", "1.00")},
		{"GET", "/wagering/transactions/0192f298-0000-7000-8000-000000000000", ""},
		{"GET", "/providers/provider-a/wagering/transactions/x", ""},
	}
	for _, rt := range routes {
		status, body := anon.do(t, rt.method, rt.path, rt.body, "Idempotency-Key", "k")
		if status != http.StatusUnauthorized || errorCode(body) != "UNAUTHENTICATED" {
			t.Errorf("%s %s without token: %d %v", rt.method, rt.path, status, body)
		}
		status, body = bad.do(t, rt.method, rt.path, rt.body, "Idempotency-Key", "k")
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s with invalid token: %d %v", rt.method, rt.path, status, body)
		}
	}
	if len(a.store.Wallets) != 1 || len(a.store.Transactions) != 1 {
		t.Fatalf("unauthenticated requests had side effects")
	}
}

func TestRolesAreEnforced(t *testing.T) {
	a := newAPI(t)
	walletID := a.openWallet(t, "10.00")
	bet := betBody(walletID, "x", "BET", "1.00")

	// Provider cannot touch wallets.
	for _, rt := range []struct{ method, path, body string }{
		{"POST", "/wallets", `{"playerId":"` + playerID + `","initialBalance":{"amount":"1.00","currency":"BRL"}}`},
		{"GET", "/wallets/" + walletID, ""},
		{"GET", "/wallets/" + walletID + "/ledger", ""},
	} {
		if status, body := a.as(tokenProviderA).do(t, rt.method, rt.path, rt.body); status != http.StatusForbidden || errorCode(body) != "FORBIDDEN" {
			t.Errorf("provider on %s %s: %d %v", rt.method, rt.path, status, body)
		}
	}
	// Internal service cannot submit operations or use provider queries.
	if status, _ := a.as(tokenInternal).do(t, "POST", "/wagering/transactions", bet, "Idempotency-Key", "k"); status != http.StatusForbidden {
		t.Errorf("internal submit: %d", status)
	}
	if status, _ := a.as(tokenInternal).do(t, "GET", "/providers/provider-a/wagering/transactions/x", ""); status != http.StatusForbidden {
		t.Errorf("internal provider query: %d", status)
	}
	// A valid token without roles is refused everywhere.
	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/wallets/" + walletID, ""},
		{"POST", "/wagering/transactions", bet},
		{"GET", "/wagering/transactions/0192f298-0000-7000-8000-000000000000", ""},
	} {
		if status, _ := a.as(tokenNoRole).do(t, rt.method, rt.path, rt.body, "Idempotency-Key", "k"); status != http.StatusForbidden {
			t.Errorf("no role on %s %s: %d", rt.method, rt.path, status)
		}
	}
	// Provider role without the provider claim cannot submit.
	if status, _ := a.as(tokenProviderNoID).do(t, "POST", "/wagering/transactions", bet, "Idempotency-Key", "k"); status != http.StatusForbidden {
		t.Errorf("provider without claim: %d", status)
	}
	if len(a.store.Transactions) != 1 {
		t.Fatalf("forbidden requests had side effects")
	}
}

func TestProviderIsolation(t *testing.T) {
	a := newAPI(t)
	walletID := a.openWallet(t, "100.00")

	// provider-b cannot submit as provider-a, even with a valid token.
	status, body := a.as(tokenProviderB).do(t, "POST", "/wagering/transactions", betBody(walletID, "tx-1", "BET", "1.00"), "Idempotency-Key", "k1")
	if status != http.StatusForbidden || !strings.Contains(body["error"].(map[string]any)["message"].(string), "provider-a") {
		t.Fatalf("submit as another provider: %d %v", status, body)
	}
	if len(a.store.Transactions) != 1 {
		t.Fatalf("forbidden submit persisted a transaction")
	}

	status, body = a.submit(t, "k1", betBody(walletID, "tx-1", "BET", "1.00"))
	if status != http.StatusOK {
		t.Fatal(status, body)
	}
	txID := body["transactionId"].(string)

	// Replay of provider-a's key by provider-b is forbidden by the body
	// check before it reaches idempotency.
	if status, _ := a.as(tokenProviderB).do(t, "POST", "/wagering/transactions", betBody(walletID, "tx-1", "BET", "1.00"), "Idempotency-Key", "k1"); status != http.StatusForbidden {
		t.Errorf("replay by another provider: %d", status)
	}
	// Queries: by internal id provider-b gets 404, provider-a and internal get 200.
	if status, _ := a.as(tokenProviderB).do(t, "GET", "/wagering/transactions/"+txID, ""); status != http.StatusNotFound {
		t.Errorf("by id as provider-b: %d", status)
	}
	if status, _ := a.as(tokenProviderA).do(t, "GET", "/wagering/transactions/"+txID, ""); status != http.StatusOK {
		t.Errorf("by id as provider-a: %d", status)
	}
	if status, _ := a.as(tokenInternal).do(t, "GET", "/wagering/transactions/"+txID, ""); status != http.StatusOK {
		t.Errorf("by id as internal: %d", status)
	}
	// Provider path must match the token.
	if status, _ := a.as(tokenProviderB).do(t, "GET", "/providers/provider-a/wagering/transactions/tx-1", ""); status != http.StatusForbidden {
		t.Errorf("provider path mismatch: %d", status)
	}
	if status, _ := a.as(tokenProviderA).do(t, "GET", "/providers/provider-a/wagering/transactions/tx-1", ""); status != http.StatusOK {
		t.Errorf("provider path match: %d", status)
	}
}

func TestBearerHeaderParsing(t *testing.T) {
	a := newAPI(t)
	for _, header := range []string{"bearer " + tokenInternal, "Bearer   " + tokenInternal} {
		req, _ := http.NewRequest("GET", a.ts.URL+"/wallets/0192f291-0000-7000-8000-000000000000", nil)
		req.Header.Set("Authorization", header)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("header %q: %d", header, res.StatusCode)
		}
	}
	for _, header := range []string{"Basic abc", "Bearer", "Bearer ", tokenInternal} {
		req, _ := http.NewRequest("GET", a.ts.URL+"/wallets/0192f291-0000-7000-8000-000000000000", nil)
		req.Header.Set("Authorization", header)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized || res.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("header %q: %d %q", header, res.StatusCode, res.Header.Get("WWW-Authenticate"))
		}
	}
}
