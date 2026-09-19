//go:build evidence

package evidence

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestConcurrencyAcrossThreeInstances(t *testing.T) {
	c := connect(t)

	// 50 identical submissions spread over the three instances.
	w := c.openWallet("100.00")
	op := operation{External: "replay", Kind: "BET", Amount: "10.00"}
	var wg sync.WaitGroup
	results := make([]response, 50)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = c.submit(c.api(i), w, op)
		}()
	}
	wg.Wait()
	fresh, replays := 0, 0
	for _, r := range results {
		if r.Status == http.StatusOK && r.Body["idempotentReplay"] == true {
			replays++
		} else if r.Status == http.StatusOK {
			fresh++
		}
	}
	debits := c.ledgerDebits(w)
	report.section("Mesma aposta enviada 50 vezes em paralelo por 3 instâncias",
		"Uma carteira com 100.00; a mesma operação (mesma chave de idempotência) submetida 50 vezes simultaneamente, distribuída por `app-1`, `app-2` e `app-3`.",
		[][2]string{
			{"Respostas 200", fmt.Sprint(fresh + replays)},
			{"Processamentos originais (`idempotentReplay=false`)", fmt.Sprint(fresh)},
			{"Replays (`idempotentReplay=true`)", fmt.Sprint(replays)},
			{"Débitos no ledger", fmt.Sprint(debits)},
			{"Saldo final", c.balance(w)},
		}, verdict(fresh == 1 && replays == 49 && debits == 1 && c.balance(w) == "90.00"))

	// Two distinct 80.00 bets on 100.00 from two different instances.
	w2 := c.openWallet("100.00")
	race := func() (p, r int) {
		var wg sync.WaitGroup
		res := make([]response, 2)
		for i, ext := range []string{"race-a", "race-b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[i] = c.submit(c.api(i+1), w2, operation{External: ext, Kind: "BET", Amount: "80.00"})
			}()
		}
		wg.Wait()
		for _, x := range res {
			if x.Status == http.StatusOK {
				p++
			} else if x.Status == http.StatusUnprocessableEntity && x.str("failureCode") == "INSUFFICIENT_BALANCE" {
				r++
			}
		}
		return
	}
	p1, r1 := race()
	p2, r2 := race()
	report.section("Duas apostas de 80.00 disputando 100.00 em instâncias diferentes",
		"`race-a` enviada para `app-2` e `race-b` para `app-3` ao mesmo tempo; depois as duas reenviadas.",
		[][2]string{
			{"1ª rodada: processadas / rejeitadas", fmt.Sprintf("%d / %d", p1, r1)},
			{"Reenvio: processadas / rejeitadas", fmt.Sprintf("%d / %d", p2, r2)},
			{"Código da rejeição", "INSUFFICIENT_BALANCE"},
			{"Débitos no ledger", fmt.Sprint(c.ledgerDebits(w2))},
			{"Saldo final", c.balance(w2)},
		}, verdict(p1 == 1 && r1 == 1 && p2 == 1 && r2 == 1 && c.ledgerDebits(w2) == 1 && c.balance(w2) == "20.00"))

	// Distinct wallets in parallel across instances.
	const wallets, perWallet = 12, 10
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = c.openWallet("100.00")
	}
	started := time.Now()
	errorsSeen := 0
	var mu sync.Mutex
	for i, w := range ws {
		for j := 0; j < perWallet; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if res := c.submit(c.api(i+j), w, operation{External: fmt.Sprintf("p%d-%d", i, j), Kind: "BET", Amount: "1.00"}); res.Status != http.StatusOK {
					mu.Lock()
					errorsSeen++
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	elapsed := time.Since(started)
	consistent := 0
	for _, w := range ws {
		if c.balance(w) == "90.00" && c.reconcile(w).Body["consistent"] == true {
			consistent++
		}
	}
	report.section("Carteiras distintas avançando em paralelo",
		fmt.Sprintf("%d carteiras × %d apostas de 1.00, todas ao mesmo tempo, distribuídas pelas três instâncias.", wallets, perWallet),
		[][2]string{
			{"Operações", fmt.Sprint(wallets * perWallet)},
			{"Erros", fmt.Sprint(errorsSeen)},
			{"Tempo total", elapsed.Round(time.Millisecond).String()},
			{"Carteiras com saldo 90.00 e reconciliação consistente", fmt.Sprintf("%d / %d", consistent, wallets)},
		}, verdict(errorsSeen == 0 && consistent == wallets))
}

func TestConsumerKilledWhileProcessing(t *testing.T) {
	c := connect(t)
	const wallets, perWallet = 5, 40
	ws := make([]wallet, wallets)
	var externals []string
	var messageIDs []string
	for i := range ws {
		ws[i] = c.openWallet("1000.00")
	}
	for j := 0; j < perWallet; j++ {
		for i, w := range ws {
			ext := fmt.Sprintf("kill-%d-%d", i, j)
			externals = append(externals, ext)
			messageIDs = append(messageIDs, c.send(w, operation{External: ext, Kind: "BET", Amount: "1.00"}))
		}
	}
	// Let consumption start on every instance, then kill one of them hard:
	// whatever it was doing stops between receive, commit and delete.
	time.Sleep(700 * time.Millisecond)
	killedAt := time.Now()
	c.compose("kill", "-s", "SIGKILL", "app-2")

	processed, rejected, other := c.waitAll(externals, 3*time.Minute)
	recovery := time.Since(killedAt)
	c.compose("start", "app-2")
	c.waitHealthy(90 * time.Second)

	okWallets := 0
	var debits int64
	for _, w := range ws {
		debits += c.ledgerDebits(w)
		if c.balance(w) == "960.00" && c.reconcile(w).Body["consistent"] == true {
			okWallets++
		}
	}
	inbox := c.inboxRows(messageIDs)
	report.section("Consumidor morto (SIGKILL) durante o processamento de mensagens SQS",
		fmt.Sprintf("%d mensagens (%d carteiras × %d apostas de 1.00) enviadas à fila FIFO; após 700ms, `docker compose kill -s SIGKILL app-2`. As mensagens que `app-2` tinha recebido e não removido voltaram à fila após o visibility timeout (30s) e foram tratadas por `app-1`/`app-3`; as já confirmadas no banco viraram replays pela inbox.", wallets*perWallet, wallets, perWallet),
		[][2]string{
			{"Mensagens processadas / rejeitadas / pendentes", fmt.Sprintf("%d / %d / %d", processed, rejected, other)},
			{"Linhas na inbox para essas mensagens", fmt.Sprint(inbox)},
			{"Débitos no ledger (esperado " + fmt.Sprint(wallets*perWallet) + ")", fmt.Sprint(debits)},
			{"Carteiras com saldo 960.00 e reconciliação consistente", fmt.Sprintf("%d / %d", okWallets, wallets)},
			{"Tempo até todas concluírem após o kill", recovery.Round(time.Second).String()},
		}, verdict(processed == wallets*perWallet && other == 0 && debits == int64(wallets*perWallet) && okWallets == wallets))
}

func TestOutboxPublisherKilledMidBatch(t *testing.T) {
	c := connect(t)
	const wallets, perWallet = 5, 30
	ws := make([]wallet, wallets)
	for i := range ws {
		ws[i] = c.openWallet("100.00")
	}
	// A burst of committed operations fills the outbox on all instances.
	var wg sync.WaitGroup
	for i, w := range ws {
		for j := 0; j < perWallet; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.submit(c.api(i+j), w, operation{External: fmt.Sprintf("pub-%d-%d", i, j), Kind: "BET", Amount: "1.00"})
			}()
		}
	}
	wg.Wait()
	// The publishers are draining now; kill one mid-flight.
	c.compose("kill", "-s", "SIGKILL", "app-3")
	killedAt := time.Now()

	deadline := time.Now().Add(90 * time.Second)
	var pending int64
	for {
		pending = 0
		for _, w := range ws {
			pending += c.pendingOutbox(w)
		}
		if pending == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	drained := time.Since(killedAt)
	c.compose("start", "app-3")
	c.waitHealthy(90 * time.Second)

	var events, maxAttempts int64
	for _, w := range ws {
		e, a := c.outboxAttempts(w)
		events += e
		if a > maxAttempts {
			maxAttempts = a
		}
	}
	report.section("Publisher da outbox morto (SIGKILL) no meio de um lote",
		fmt.Sprintf("%d operações commitadas geram %d eventos na outbox (Processed + BalanceChanged por operação, mais os da abertura). Com os três publishers drenando, `docker compose kill -s SIGKILL app-3`. Linhas reivindicadas por `app-3` (FOR UPDATE SKIP LOCKED) foram liberadas pelo abort da transação e assumidas pelas outras instâncias; um evento publicado antes do commit da marcação é republicado com o mesmo `eventId` (deduplicado pela fila).", wallets*perWallet, wallets*perWallet*2+wallets*2),
		[][2]string{
			{"Eventos gerados para as carteiras do cenário", fmt.Sprint(events)},
			{"Eventos ainda pendentes ao final", fmt.Sprint(pending)},
			{"Maior número de tentativas registrado", fmt.Sprint(maxAttempts)},
			{"Tempo até a outbox esvaziar após o kill", drained.Round(time.Millisecond).String()},
		}, verdict(pending == 0 && events == int64(wallets*perWallet*2+wallets*2)))
}

func TestRestartWithPendingReference(t *testing.T) {
	c := connect(t)
	w := c.openWallet("100.00")
	parked := c.submit(c.api(0), w, operation{External: "late-refund", Kind: "REFUND", Amount: "30.00", Reference: "late-bet"})
	if parked.Status != http.StatusAccepted {
		t.Fatalf("park: %d %v %v", parked.Status, parked.Body, parked.Err)
	}
	c.compose("restart", "app-1")
	// While app-1 restarts, the bet arrives through app-2.
	bet := c.submit(c.api(1), w, operation{External: "late-bet", Kind: "BET", Amount: "30.00"})
	c.waitHealthy(90 * time.Second)
	processed, _, other := c.waitAll([]string{"late-refund"}, 60*time.Second)
	replay := c.submit(c.api(0), w, operation{External: "late-refund", Kind: "REFUND", Amount: "30.00", Reference: "late-bet"})
	rec := c.reconcile(w)
	report.section("Reinício da instância com uma reversão pendente",
		"REFUND registrado como `PENDING_REFERENCE` em `app-1`; `docker compose restart app-1`; a BET referenciada chega por `app-2`; o worker de referências de qualquer instância retoma a pendência.",
		[][2]string{
			{"Estado inicial do REFUND", parked.str("status")},
			{"BET durante o reinício", fmt.Sprintf("%d %s", bet.Status, bet.str("status"))},
			{"REFUND após o reinício", map[bool]string{true: "PROCESSED", false: "ainda pendente"}[processed == 1]},
			{"Replay do REFUND em `app-1` reiniciada", fmt.Sprintf("%d idempotentReplay=%v", replay.Status, replay.Body["idempotentReplay"])},
			{"Saldo final / reconciliação", fmt.Sprintf("%s / consistente=%v", c.balance(w), rec.Body["consistent"])},
		}, verdict(processed == 1 && other == 0 && c.balance(w) == "100.00" && rec.Body["consistent"] == true && replay.Body["idempotentReplay"] == true))
}

func TestPostgresOutageDuringTraffic(t *testing.T) {
	c := connect(t)
	w := c.openWallet("1000.00")
	var externals []string
	for j := 0; j < 30; j++ {
		ext := fmt.Sprintf("outage-sqs-%d", j)
		externals = append(externals, ext)
		c.send(w, operation{External: ext, Kind: "BET", Amount: "1.00"})
	}
	c.compose("pause", "postgres")
	var unavailable, ok int
	var httpExternals []string
	var wg sync.WaitGroup
	var mu sync.Mutex
	for j := 0; j < 10; j++ {
		ext := fmt.Sprintf("outage-http-%d", j)
		httpExternals = append(httpExternals, ext)
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := c.submit(c.api(j), w, operation{External: ext, Kind: "BET", Amount: "1.00"})
			mu.Lock()
			defer mu.Unlock()
			switch res.Status {
			case http.StatusServiceUnavailable:
				unavailable++
			case http.StatusOK:
				ok++
			}
		}()
	}
	time.Sleep(12 * time.Second)
	c.compose("unpause", "postgres")
	wg.Wait()

	// Everything sent over SQS lands once the database is back; HTTP
	// callers that got 503 simply resend.
	resent := 0
	for j, ext := range httpExternals {
		if res := c.submit(c.api(j), w, operation{External: ext, Kind: "BET", Amount: "1.00"}); res.Status == http.StatusOK {
			resent++
		}
	}
	processed, _, other := c.waitAll(externals, 3*time.Minute)
	rec := c.reconcile(w)
	report.section("PostgreSQL indisponível durante o tráfego",
		"30 mensagens na fila e 10 requisições HTTP concorrentes com o PostgreSQL pausado (`docker compose pause postgres`) por 12s: as conexões TCP congelam sem erro, então o que protege o serviço é o timeout por requisição (10s → 503) e o backoff de reentrega do consumidor. Nada é aplicado duas vezes; HTTP responde 503 e o cliente reenvia.",
		[][2]string{
			{"HTTP durante a pausa: 503 / 200", fmt.Sprintf("%d / %d", unavailable, ok)},
			{"Reenvios HTTP após a volta que resultaram em 200", fmt.Sprintf("%d / 10", resent)},
			{"Mensagens SQS processadas / pendentes", fmt.Sprintf("%d / %d", processed, other)},
			{"Débitos no ledger (esperado 40)", fmt.Sprint(c.ledgerDebits(w))},
			{"Saldo final / reconciliação", fmt.Sprintf("%s / consistente=%v", c.balance(w), rec.Body["consistent"])},
		}, verdict(processed == 30 && other == 0 && resent == 10 && c.ledgerDebits(w) == 40 && c.balance(w) == "960.00" && rec.Body["consistent"] == true))
}

func TestGracefulStopUnderLoad(t *testing.T) {
	c := connect(t)
	w := c.openWallet("1000.00")
	var externals []string
	for j := 0; j < 60; j++ {
		ext := fmt.Sprintf("stop-sqs-%d", j)
		externals = append(externals, ext)
		c.send(w, operation{External: ext, Kind: "BET", Amount: "1.00"})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount, connErrors := 0, 0
	for j := 0; j < 40; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := c.submit(c.apis[1], w, operation{External: fmt.Sprintf("stop-http-%d", j), Kind: "BET", Amount: "1.00"})
			mu.Lock()
			defer mu.Unlock()
			if res.Status == http.StatusOK {
				okCount++
			} else if res.Err != nil {
				connErrors++
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	stoppedAt := time.Now()
	c.compose("stop", "-t", "20", "app-2") // SIGTERM, up to 20s to drain
	stopTook := time.Since(stoppedAt)
	wg.Wait()
	c.compose("start", "app-2")
	c.waitHealthy(90 * time.Second)

	processed, _, other := c.waitAll(externals, 2*time.Minute)
	rec := c.reconcile(w)
	report.section("Parada graciosa (SIGTERM) de uma instância sob carga",
		"60 mensagens na fila e 40 requisições HTTP concorrentes contra `app-2`; `docker compose stop app-2` (SIGTERM) 150ms depois. A instância para de aceitar conexões e de buscar mensagens, conclui o que está em curso e fecha o pool por último.",
		[][2]string{
			{"HTTP contra a instância parando: 200 / recusadas", fmt.Sprintf("%d / %d", okCount, connErrors)},
			{"Tempo do `docker compose stop`", stopTook.Round(time.Millisecond).String()},
			{"Mensagens SQS processadas / pendentes", fmt.Sprintf("%d / %d", processed, other)},
			{"Débitos no ledger (esperado " + fmt.Sprint(60+okCount) + ")", fmt.Sprint(c.ledgerDebits(w))},
			{"Reconciliação consistente", fmt.Sprint(rec.Body["consistent"])},
		}, verdict(processed == 60 && other == 0 && c.ledgerDebits(w) == int64(60+okCount) && rec.Body["consistent"] == true))
}
