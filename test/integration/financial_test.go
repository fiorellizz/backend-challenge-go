//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestSchemaEnforcesFinancialInvariants(t *testing.T) {
	s := start(t, nil)
	ctx := context.Background()

	var version int
	var dirty bool
	if err := s.pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 1 || dirty {
		t.Fatalf("migrations: version %d dirty %v err %v", version, dirty, err)
	}

	w := s.openWallet("100.00")
	s.submit(w, operation{External: "bet-1", Kind: "BET", Amount: "30.00"})

	if _, err := s.pool.Exec(ctx, `UPDATE wallet_ledger_entries SET amount_minor = 1 WHERE wallet_id = $1`, w.ID); err == nil {
		t.Error("ledger UPDATE must be refused by the trigger")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM wallet_ledger_entries WHERE wallet_id = $1`, w.ID); err == nil {
		t.Error("ledger DELETE must be refused by the trigger")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE wallets SET balance_minor = -1 WHERE id = $1`, w.ID); err == nil {
		t.Error("negative balance must be refused by the CHECK constraint")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE wallets SET version = 0 WHERE id = $1`, w.ID); err == nil {
		t.Error("version below 1 must be refused")
	}

	// A second successful reversal of the same reference is impossible
	// even if the application logic were bypassed.
	s.submit(w, operation{External: "refund-1", Kind: "REFUND", Amount: "30.00", Reference: "bet-1"})
	_, err := s.pool.Exec(ctx, `
		INSERT INTO wager_transactions (id, origin, provider_id, external_transaction_id, idempotency_key, payload_hash,
			round_id, game_id, wallet_id, player_id, kind, amount_minor, currency, reference_external_transaction_id,
			reference_transaction_id, status, balance_after_minor, processed_at)
		SELECT gen_random_uuid(), 'EXTERNAL', 'provider-a', $2, $2, '\x01', 'r', 'g', wallet_id, player_id,
		       'ROLLBACK', amount_minor, currency, reference_external_transaction_id, reference_transaction_id, 'PROCESSED', 0, now()
		  FROM wager_transactions WHERE provider_id = 'provider-a' AND external_transaction_id = $3 AND wallet_id = $1`,
		w.ID, s.ext("forced"), s.ext("refund-1"))
	if err == nil {
		t.Error("second successful reversal must be refused by the partial unique index")
	}
	s.assertConsistent(w)
}

func TestFiftyParallelReplaysProduceOneDebit(t *testing.T) {
	s := start(t, nil)
	w := s.openWallet("100.00")
	op := operation{External: "same-bet", Kind: "BET", Amount: "10.00"}

	var wg sync.WaitGroup
	results := make([]response, 50)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = s.submit(w, op)
		}()
	}
	wg.Wait()

	fresh, replays := 0, 0
	for _, r := range results {
		if r.Status != http.StatusOK || r.str("status") != "PROCESSED" || r.amount("balance") != "90.00" {
			t.Fatalf("unexpected response: %d %v", r.Status, r.Body)
		}
		if r.Body["idempotentReplay"] == true {
			replays++
		} else {
			fresh++
		}
	}
	if fresh != 1 || replays != 49 {
		t.Fatalf("fresh=%d replays=%d", fresh, replays)
	}
	if got := s.balance(w); got != "90.00" {
		t.Fatalf("balance %s", got)
	}
	if debits, _ := s.ledgerStats(w); debits != 1 {
		t.Fatalf("debits = %d", debits)
	}
	s.assertConsistent(w)
}

func TestTwoBetsRacingForTheSameBalance(t *testing.T) {
	s := start(t, nil)
	w := s.openWallet("100.00")
	ops := []operation{{External: "bet-a", Kind: "BET", Amount: "80.00"}, {External: "bet-b", Kind: "BET", Amount: "80.00"}}

	run := func() (processed, rejected int) {
		var wg sync.WaitGroup
		results := make([]response, 2)
		for i, op := range ops {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = s.submit(w, op)
			}()
		}
		wg.Wait()
		for _, r := range results {
			switch {
			case r.Status == http.StatusOK && r.str("status") == "PROCESSED" && r.amount("balance") == "20.00":
				processed++
			case r.Status == http.StatusUnprocessableEntity && r.str("failureCode") == "INSUFFICIENT_BALANCE":
				rejected++
			default:
				t.Fatalf("unexpected response: %d %v", r.Status, r.Body)
			}
		}
		return
	}

	if p, r := run(); p != 1 || r != 1 {
		t.Fatalf("processed=%d rejected=%d", p, r)
	}
	// Resending both must not change anything.
	if p, r := run(); p != 1 || r != 1 {
		t.Fatalf("resend: processed=%d rejected=%d", p, r)
	}
	if got := s.balance(w); got != "20.00" {
		t.Fatalf("balance %s", got)
	}
	if debits, _ := s.ledgerStats(w); debits != 1 {
		t.Fatalf("debits = %d", debits)
	}
	s.assertConsistent(w)
}

func TestDistinctWalletsProceedInParallel(t *testing.T) {
	s := start(t, nil)
	const wallets, betsPerWallet = 12, 8
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = s.openWallet("100.00")
	}

	started := time.Now()
	var wg sync.WaitGroup
	for i, w := range ws {
		for j := 0; j < betsPerWallet; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res := s.submit(w, operation{External: fmt.Sprintf("w%d-bet%d", i, j), Kind: "BET", Amount: "5.00"})
				if res.Status != http.StatusOK {
					t.Errorf("wallet %d bet %d: %d %v", i, j, res.Status, res.Body)
				}
			}()
		}
	}
	wg.Wait()
	elapsed := time.Since(started)

	for i, w := range ws {
		if got := s.balance(w); got != "60.00" {
			t.Errorf("wallet %d balance %s", i, got)
		}
		s.assertConsistent(w)
	}
	// 96 operations across 12 wallets; with a global lock they would take
	// far longer than this on the same machine.
	if elapsed > 20*time.Second {
		t.Fatalf("%d operations took %s", wallets*betsPerWallet, elapsed)
	}
	t.Logf("%d operations on %d wallets in %s", wallets*betsPerWallet, wallets, elapsed)
}

func TestReversalsEndToEnd(t *testing.T) {
	s := start(t, nil)
	w := s.openWallet("100.00")
	s.submit(w, operation{External: "bet-1", Kind: "BET", Amount: "40.00"})
	s.submit(w, operation{External: "win-1", Kind: "WIN", Amount: "10.00"})

	if res := s.submit(w, operation{External: "refund-1", Kind: "REFUND", Amount: "40.00", Reference: "bet-1"}); res.Status != http.StatusOK || res.amount("balance") != "110.00" {
		t.Fatalf("refund: %d %v", res.Status, res.Body)
	}
	if res := s.submit(w, operation{External: "rb-1", Kind: "ROLLBACK", Amount: "40.00", Reference: "bet-1"}); res.Status != http.StatusUnprocessableEntity || res.str("failureCode") != "REFERENCE_ALREADY_REVERSED" {
		t.Fatalf("second reversal: %d %v", res.Status, res.Body)
	}
	if res := s.submit(w, operation{External: "rb-win", Kind: "ROLLBACK", Amount: "10.00", Reference: "win-1"}); res.Status != http.StatusOK || res.amount("balance") != "100.00" {
		t.Fatalf("rollback win: %d %v", res.Status, res.Body)
	}
	if res := s.submit(w, operation{External: "loss-1", Kind: "LOSS", Amount: "0.00"}); res.Status != http.StatusOK || res.amount("balance") != "100.00" {
		t.Fatalf("loss: %d %v", res.Status, res.Body)
	}
	debits, credits := s.ledgerStats(w)
	if debits != 2 || credits != 3 { // bet, rollback-win | opening, win, refund
		t.Fatalf("ledger debits=%d credits=%d", debits, credits)
	}
	s.assertConsistent(w)
}

func TestRefundBeforeBetIsResolvedByTheWorker(t *testing.T) {
	s := start(t, nil)
	w := s.openWallet("100.00")

	res := s.submit(w, operation{External: "refund-late", Kind: "REFUND", Amount: "25.00", Reference: "bet-late"})
	if res.Status != http.StatusAccepted || res.str("status") != "PENDING_REFERENCE" {
		t.Fatalf("parked refund: %d %v", res.Status, res.Body)
	}
	if res := s.submit(w, operation{External: "refund-late", Kind: "REFUND", Amount: "25.00", Reference: "bet-late"}); res.Status != http.StatusAccepted || res.Body["idempotentReplay"] != true {
		t.Fatalf("replay while parked: %d %v", res.Status, res.Body)
	}
	if got := s.balance(w); got != "100.00" {
		t.Fatalf("parked refund moved money: %s", got)
	}

	s.submit(w, operation{External: "bet-late", Kind: "BET", Amount: "25.00"})
	final := s.waitStatus("provider-a", "refund-late", 15*time.Second, "PROCESSED", "REJECTED")
	if final.str("status") != "PROCESSED" || final.amount("balance") != "100.00" {
		t.Fatalf("resolved refund: %v", final.Body)
	}
	if got := s.balance(w); got != "100.00" {
		t.Fatalf("balance %s", got)
	}
	s.assertConsistent(w)
}

func TestReversalWithoutReferenceExpiresAsRejected(t *testing.T) {
	s := start(t, map[string]string{"REFERENCE_MAX_ATTEMPTS": "2", "REFERENCE_BASE_BACKOFF": "300ms", "REFERENCE_TTL": "10s"})
	w := s.openWallet("100.00")
	if res := s.submit(w, operation{External: "rb-never", Kind: "ROLLBACK", Amount: "1.00", Reference: "bet-never"}); res.Status != http.StatusAccepted {
		t.Fatalf("park: %d %v", res.Status, res.Body)
	}
	final := s.waitStatus("provider-a", "rb-never", 15*time.Second, "REJECTED")
	if final.str("failureCode") != "REFERENCE_NOT_FOUND" {
		t.Fatalf("expired reversal: %v", final.Body)
	}
	if res := s.submit(w, operation{External: "rb-never", Kind: "ROLLBACK", Amount: "1.00", Reference: "bet-never"}); res.Status != http.StatusUnprocessableEntity || res.Body["idempotentReplay"] != true {
		t.Fatalf("replay after expiry: %d %v", res.Status, res.Body)
	}
	s.assertConsistent(w)
}
