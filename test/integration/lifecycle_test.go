//go:build integration

package integration

import (
	"net/http"
	"testing"
	"time"
)

func TestAuthenticationAndProviderIsolation(t *testing.T) {
	s := start(t, nil)
	w := s.openWallet("100.00")
	bet := s.scoped(operation{External: "auth-bet", Kind: "BET", Amount: "1.00"})
	body := bet.body(w)

	// Missing, garbage and tampered tokens are refused everywhere.
	valid := s.token("provider-a")
	tampered := valid[:len(valid)-4] + "AAAA"
	for _, tok := range []string{"", "not-a-token", tampered} {
		if res := s.call("POST", "/wagering/transactions", tok, body, "Idempotency-Key", "k"); res.Status != http.StatusUnauthorized || res.errorCode() != "UNAUTHENTICATED" {
			t.Errorf("token %q: %d %v", tok[:min(len(tok), 8)], res.Status, res.Body)
		}
		if res := s.call("GET", "/wallets/"+w.ID, tok, ""); res.Status != http.StatusUnauthorized {
			t.Errorf("wallet with token %q: %d", tok[:min(len(tok), 8)], res.Status)
		}
	}
	// A real token without any role is forbidden.
	if res := s.call("POST", "/wagering/transactions", s.token("unauthorized-client"), body, "Idempotency-Key", "k"); res.Status != http.StatusForbidden {
		t.Errorf("unauthorized client: %d %v", res.Status, res.Body)
	}
	// Roles are enforced in both directions.
	if res := s.call("POST", "/wallets", s.token("provider-a"), `{"playerId":"`+w.PlayerID+`","initialBalance":{"amount":"1.00","currency":"BRL"}}`); res.Status != http.StatusForbidden {
		t.Errorf("provider opening wallet: %d", res.Status)
	}
	if res := s.call("POST", "/wagering/transactions", s.token("wallet-internal"), body, "Idempotency-Key", "k"); res.Status != http.StatusForbidden {
		t.Errorf("internal submitting: %d", res.Status)
	}
	if got := s.balance(w); got != "100.00" {
		t.Fatalf("unauthorized requests moved money: %s", got)
	}

	// provider-b cannot act as, replay or read provider-a.
	res := s.call("POST", "/wagering/transactions", s.token("provider-a"), body, "Idempotency-Key", bet.key())
	txID := res.str("transactionId")
	if res := s.call("POST", "/wagering/transactions", s.token("provider-b"), body, "Idempotency-Key", bet.key()); res.Status != http.StatusForbidden {
		t.Errorf("provider-b replaying provider-a: %d %v", res.Status, res.Body)
	}
	if res := s.call("GET", "/providers/provider-a/wagering/transactions/"+bet.External, s.token("provider-b"), ""); res.Status != http.StatusForbidden {
		t.Errorf("provider-b on provider-a path: %d", res.Status)
	}
	if res := s.call("GET", "/wagering/transactions/"+txID, s.token("provider-b"), ""); res.Status != http.StatusNotFound {
		t.Errorf("provider-b by id: %d", res.Status)
	}
	if res := s.call("GET", "/wagering/transactions/"+txID, s.token("provider-a"), ""); res.Status != http.StatusOK {
		t.Errorf("provider-a by id: %d", res.Status)
	}
	if res := s.call("GET", "/wagering/transactions/"+txID, s.token("wallet-internal"), ""); res.Status != http.StatusOK {
		t.Errorf("internal by id: %d", res.Status)
	}
	// provider-b submitting its own operation on the same wallet works,
	// and each provider only sees its own external ids.
	if res := s.submit(w, operation{Provider: "provider-b", External: "b-bet", Kind: "BET", Amount: "1.00"}); res.Status != http.StatusOK {
		t.Errorf("provider-b own operation: %d %v", res.Status, res.Body)
	}
	if res := s.providerTransaction("provider-a", "b-bet"); res.Status != http.StatusNotFound {
		t.Errorf("provider-a reading provider-b's operation: %d", res.Status)
	}
	if got := s.balance(w); got != "98.00" {
		t.Fatalf("balance %s", got)
	}
}

func TestRestartPreservesIdempotencyPendingWorkAndConsistency(t *testing.T) {
	first := start(t, map[string]string{"APP_INSTANCE_ID": "restart-1"})
	w := first.openWallet("100.00")
	bet := operation{External: "restart-bet", Kind: "BET", Amount: "30.00"}
	original := first.submit(w, bet)
	if original.Status != http.StatusOK {
		t.Fatalf("bet: %d %v", original.Status, original.Body)
	}
	parked := first.submit(w, operation{External: "restart-refund", Kind: "REFUND", Amount: "30.00", Reference: "bet-after-restart"})
	if parked.Status != http.StatusAccepted {
		t.Fatalf("park: %d %v", parked.Status, parked.Body)
	}
	first.stop()

	// A fresh process with its own connections and memory takes over.
	second := start(t, map[string]string{"APP_INSTANCE_ID": "restart-2"})
	second.run = first.run // same external ids as the first process
	replay := second.submit(w, bet)
	if replay.Status != http.StatusOK || replay.Body["idempotentReplay"] != true || replay.str("transactionId") != original.str("transactionId") || replay.amount("balance") != "70.00" {
		t.Fatalf("replay after restart: %d %v", replay.Status, replay.Body)
	}
	if res := second.submit(w, operation{External: "restart-refund", Kind: "REFUND", Amount: "30.00", Reference: "bet-after-restart"}); res.Status != http.StatusAccepted || res.Body["idempotentReplay"] != true {
		t.Fatalf("parked replay after restart: %d %v", res.Status, res.Body)
	}

	// The pending work is resumed by the new instance once its reference
	// arrives.
	second.submit(w, operation{External: "bet-after-restart", Kind: "BET", Amount: "30.00"})
	final := second.waitStatus("provider-a", "restart-refund", 15*time.Second, "PROCESSED", "REJECTED")
	if final.str("status") != "PROCESSED" {
		t.Fatalf("resumed refund: %v", final.Body)
	}
	if got := second.balance(w); got != "70.00" {
		t.Fatalf("balance %s", got)
	}
	second.assertConsistent(w)
}

func TestFxCompositionStartsAndStopsEveryRole(t *testing.T) {
	for _, roles := range []string{"api,consumer,outbox", "api", "consumer", "outbox"} {
		t.Run(roles, func(t *testing.T) {
			s := start(t, map[string]string{"APP_ROLES": roles})
			res := s.call("GET", "/health/ready", "", "")
			if res.Status != http.StatusOK {
				t.Fatalf("ready: %d %v", res.Status, res.Body)
			}
			// Business routes are mounted only with the api role; without it
			// the mux answers with its plain-text 404.
			req, _ := http.NewRequest("GET", s.base+"/wallets/0192f291-0000-7000-8000-000000000000", nil)
			req.Header.Set("Authorization", "Bearer "+s.token("wallet-internal"))
			raw, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw.Body.Close()
			isJSON := raw.Header.Get("Content-Type") == "application/json"
			hasAPI := roles == "api,consumer,outbox" || roles == "api"
			if raw.StatusCode != http.StatusNotFound || isJSON != hasAPI {
				t.Fatalf("wallet route with roles %s: %d json=%v", roles, raw.StatusCode, isJSON)
			}
			started := time.Now()
			s.stop()
			t.Logf("roles %s stopped in %s", roles, time.Since(started))
		})
	}
}
