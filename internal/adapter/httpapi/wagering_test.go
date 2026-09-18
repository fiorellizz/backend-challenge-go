package httpapi_test

import (
	"fmt"
	"net/http"
	"testing"
)

func betBody(walletID, external, kind, amount string) string {
	return fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":"%s","playerId":"%s","walletId":"%s","roundId":"round-987","gameId":"fortune-chimp","kind":"%s","money":{"amount":"%s","currency":"BRL"}}`,
		external, playerID, walletID, kind, amount)
}

func reversalBody(walletID, external, kind, reference, amount string) string {
	return fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":"%s","playerId":"%s","walletId":"%s","roundId":"round-987","gameId":"fortune-chimp","kind":"%s","money":{"amount":"%s","currency":"BRL"},"referenceExternalTransactionId":"%s"}`,
		external, playerID, walletID, kind, amount, reference)
}

// submit acts as provider-a, then restores the previous identity.
func (a *api) submit(t *testing.T, key, body string) (int, map[string]any) {
	t.Helper()
	return a.as(tokenProviderA).do(t, "POST", "/wagering/transactions", body, "Idempotency-Key", key)
}

// as returns a view of the API that authenticates with the given token.
func (a *api) as(token string) *api {
	return &api{ts: a.ts, store: a.store, token: token}
}

func TestSubmitBetProcessed(t *testing.T) {
	a := newAPI(t)
	walletID := a.openWallet(t, "1000.00")

	status, body := a.submit(t, "provider-a:transaction-123", betBody(walletID, "transaction-123", "BET", "25.00"))
	if status != http.StatusOK {
		t.Fatalf("status = %d %v", status, body)
	}
	if body["status"] != "PROCESSED" || body["idempotentReplay"] != false || body["balance"].(map[string]any)["amount"] != "975.00" {
		t.Fatalf("body = %v", body)
	}
	if _, has := body["failureCode"]; has {
		t.Fatalf("processed response must not carry failureCode")
	}
	txID := body["transactionId"].(string)

	status, body = a.do(t, "GET", "/wagering/transactions/"+txID, "")
	if status != http.StatusOK || body["status"] != "PROCESSED" || body["kind"] != "BET" || body["walletVersion"] != float64(2) || body["externalTransactionId"] != "transaction-123" {
		t.Fatalf("get by id: %d %v", status, body)
	}
	status, body = a.as(tokenProviderA).do(t, "GET", "/providers/provider-a/wagering/transactions/transaction-123", "")
	if status != http.StatusOK || body["transactionId"] != txID {
		t.Fatalf("get by provider: %d %v", status, body)
	}
	status, body = a.as(tokenProviderB).do(t, "GET", "/providers/provider-b/wagering/transactions/transaction-123", "")
	if status != http.StatusNotFound {
		t.Fatalf("other provider must not see it: %d %v", status, body)
	}
}

func TestSubmitReplayAndConflicts(t *testing.T) {
	a := newAPI(t)
	walletID := a.openWallet(t, "1000.00")
	body := betBody(walletID, "tx-1", "BET", "25.00")
	a.submit(t, "k1", body)
	a.submit(t, "k2", betBody(walletID, "tx-2", "BET", "100.00"))

	status, res := a.submit(t, "k1", body)
	if status != http.StatusOK || res["idempotentReplay"] != true || res["balance"].(map[string]any)["amount"] != "975.00" {
		t.Fatalf("replay: %d %v", status, res)
	}

	status, res = a.submit(t, "k1", betBody(walletID, "tx-1", "BET", "26.00"))
	if status != http.StatusConflict || errorCode(res) != "CONFLICT" {
		t.Fatalf("same key other payload: %d %v", status, res)
	}
	status, res = a.submit(t, "k-other", body)
	if status != http.StatusConflict {
		t.Fatalf("same operation other key: %d %v", status, res)
	}

	status, res = a.as(tokenProviderA).do(t, "POST", "/wagering/transactions", body)
	if status != http.StatusBadRequest || errorCode(res) != "VALIDATION_ERROR" {
		t.Fatalf("missing Idempotency-Key: %d %v", status, res)
	}
}

func TestSubmitRejectedIs422WithFailureCode(t *testing.T) {
	a := newAPI(t)
	walletID := a.openWallet(t, "10.00")
	status, body := a.submit(t, "k1", betBody(walletID, "tx-1", "BET", "50.00"))
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d %v", status, body)
	}
	if body["status"] != "REJECTED" || body["failureCode"] != "INSUFFICIENT_BALANCE" || errorCode(body) != "REJECTED" {
		t.Fatalf("body = %v", body)
	}
	if _, has := body["balance"]; has {
		t.Fatalf("rejected response must not carry a balance")
	}
	status, body = a.do(t, "GET", "/wagering/transactions/"+body["transactionId"].(string), "")
	if status != http.StatusOK || body["failureCode"] != "INSUFFICIENT_BALANCE" {
		t.Fatalf("query rejected: %d %v", status, body)
	}
}

func TestSubmitReversalBeforeReferenceIs202(t *testing.T) {
	a := newAPI(t)
	walletID := a.openWallet(t, "100.00")
	status, body := a.submit(t, "k-refund", reversalBody(walletID, "refund-1", "REFUND", "bet-late", "30.00"))
	if status != http.StatusAccepted || body["status"] != "PENDING_REFERENCE" || body["nextAttemptAt"] == nil {
		t.Fatalf("status = %d %v", status, body)
	}
	status, body = a.do(t, "GET", "/wagering/transactions/"+body["transactionId"].(string), "")
	if status != http.StatusOK || body["status"] != "PENDING_REFERENCE" || body["referenceExternalTransactionId"] != "bet-late" {
		t.Fatalf("query pending: %d %v", status, body)
	}
}

func TestSubmitLossAndWin(t *testing.T) {
	a := newAPI(t)
	walletID := a.openWallet(t, "100.00")
	status, body := a.submit(t, "k-loss", betBody(walletID, "loss-1", "LOSS", "0.00"))
	if status != http.StatusOK || body["balance"].(map[string]any)["amount"] != "100.00" {
		t.Fatalf("loss: %d %v", status, body)
	}
	status, body = a.submit(t, "k-win", betBody(walletID, "win-1", "WIN", "5.50"))
	if status != http.StatusOK || body["balance"].(map[string]any)["amount"] != "105.50" {
		t.Fatalf("win: %d %v", status, body)
	}
}

func TestSubmitValidation(t *testing.T) {
	a := newAPI(t)
	walletID := a.openWallet(t, "100.00")
	cases := map[string]string{
		"opening kind":        betBody(walletID, "x", "OPENING", "1.00"),
		"unknown kind":        betBody(walletID, "x", "JACKPOT", "1.00"),
		"loss with amount":    betBody(walletID, "x", "LOSS", "1.00"),
		"bet with zero":       betBody(walletID, "x", "BET", "0.00"),
		"negative amount":     betBody(walletID, "x", "BET", "-1.00"),
		"scientific notation": betBody(walletID, "x", "BET", "1e2"),
		"refund without ref":  betBody(walletID, "x", "REFUND", "1.00"),
		"bad wallet id":       betBody("nope", "x", "BET", "1.00"),
		"unknown field":       `{"providerId":"provider-a","foo":1}`,
		"empty external id":   betBody(walletID, "", "BET", "1.00"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			status, res := a.submit(t, "k-"+name, body)
			if status != http.StatusBadRequest || errorCode(res) != "VALIDATION_ERROR" {
				t.Fatalf("status = %d %v", status, res)
			}
		})
	}
	status, res := a.submit(t, "k-404", betBody("0192f291-0000-7000-8000-000000000000", "x", "BET", "1.00"))
	if status != http.StatusNotFound {
		t.Fatalf("unknown wallet: %d %v", status, res)
	}
	if len(a.store.Transactions) != 1 {
		t.Fatalf("invalid submissions persisted transactions: %d", len(a.store.Transactions))
	}
}

func TestGetTransactionNotFound(t *testing.T) {
	a := newAPI(t)
	if status, _ := a.do(t, "GET", "/wagering/transactions/0192f298-0000-7000-8000-000000000000", ""); status != http.StatusNotFound {
		t.Fatalf("status = %d", status)
	}
	if status, _ := a.do(t, "GET", "/wagering/transactions/nope", ""); status != http.StatusBadRequest {
		t.Fatalf("status = %d", status)
	}
}
