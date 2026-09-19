//go:build evidence

// Package evidence drives the three application instances of the Docker
// Compose stack (real processes, each with its own connections and
// memory) through failure scenarios and records what happened in
// EVIDENCE.md. Run with `make evidence`.
package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cluster is the compose stack as seen from the host.
type cluster struct {
	t      *testing.T
	apis   []string
	pool   *pgxpool.Pool
	sqs    *sqs.Client
	queue  string
	issuer string
	mu     sync.Mutex
	tokens map[string]string
	run    string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func connect(t *testing.T) *cluster {
	t.Helper()
	if os.Getenv("EVIDENCE") == "" {
		t.Skip("set EVIDENCE=1 with the compose stack up (make evidence)")
	}
	c := &cluster{
		t:      t,
		apis:   strings.Split(env("EVIDENCE_APIS", "http://localhost:8081,http://localhost:8082,http://localhost:8083"), ","),
		queue:  env("SQS_WAGER_QUEUE_URL", "http://localhost:4566/000000000000/wager-transactions.fifo"),
		issuer: env("OIDC_ISSUER_URL", "http://localhost:8080/realms/wager"),
		tokens: map[string]string{},
		run:    uuid.NewString()[:8],
	}
	pool, err := pgxpool.New(context.Background(), env("DATABASE_URL", "postgres://wager:wager@localhost:5433/wager?sslmode=disable"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	c.pool = pool

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(env("AWS_REGION", "us-east-1")))
	if err != nil {
		t.Fatal(err)
	}
	c.sqs = sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		o.BaseEndpoint = aws.String(env("AWS_ENDPOINT_URL", "http://localhost:4566"))
	})
	c.waitHealthy(60 * time.Second)
	return c
}

// compose runs a docker compose command against the stack, retrying a
// few times: the daemon occasionally refuses a start issued right after a
// stop while it is still tearing the container down.
func (c *cluster) compose(args ...string) {
	c.t.Helper()
	var out []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
		cmd.Dir = env("EVIDENCE_COMPOSE_DIR", "../..")
		if out, err = cmd.CombinedOutput(); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	c.t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
}

// waitHealthy blocks until every instance answers /health/ready.
func (c *cluster) waitHealthy(timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for _, api := range c.apis {
		for {
			res, err := http.Get(api + "/health/ready")
			if err == nil {
				res.Body.Close()
				if res.StatusCode == http.StatusOK {
					break
				}
			}
			if time.Now().After(deadline) {
				c.t.Fatalf("%s never became ready", api)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func (c *cluster) ext(name string) string { return c.run + "-" + name }

func (c *cluster) token(clientID string) string {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if tok, ok := c.tokens[clientID]; ok {
		return tok
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientID + "-secret"}}
	res, err := http.PostForm(c.issuer+"/protocol/openid-connect/token", form)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if res.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil {
		c.t.Fatalf("token: %d %s", res.StatusCode, body)
	}
	c.tokens[clientID] = out.AccessToken
	return out.AccessToken
}

type response struct {
	Status int
	Body   map[string]any
	Err    error
}

func (r response) str(key string) string { v, _ := r.Body[key].(string); return v }

func (r response) amount(key string) string {
	m, _ := r.Body[key].(map[string]any)
	v, _ := m["amount"].(string)
	return v
}

// call sends one request to the given instance. Connection errors are
// returned, not fatal: killed instances are part of the scenarios.
func (c *cluster) call(api, method, path, token, body string, headers ...string) response {
	req, err := http.NewRequest(method, api+path, bytes.NewBufferString(body))
	if err != nil {
		return response{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return response{Err: err}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := response{Status: res.StatusCode}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out.Body)
	}
	return out
}

func (c *cluster) api(i int) string { return c.apis[i%len(c.apis)] }

type wallet struct{ ID, PlayerID string }

func (c *cluster) openWallet(amount string) wallet {
	c.t.Helper()
	player := uuid.NewString()
	res := c.call(c.api(0), "POST", "/wallets", c.token("wallet-internal"),
		fmt.Sprintf(`{"playerId":"%s","initialBalance":{"amount":"%s","currency":"BRL"}}`, player, amount))
	if res.Status != http.StatusCreated {
		c.t.Fatalf("open wallet: %d %v %v", res.Status, res.Body, res.Err)
	}
	return wallet{ID: res.str("id"), PlayerID: player}
}

type operation struct {
	External, Kind, Amount, Reference string
}

func (c *cluster) body(w wallet, op operation) string {
	ref := ""
	if op.Reference != "" {
		ref = fmt.Sprintf(`,"referenceExternalTransactionId":"%s"`, c.ext(op.Reference))
	}
	return fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":"%s","playerId":"%s","walletId":"%s","roundId":"round-1","gameId":"fortune-chimp","kind":"%s","money":{"amount":"%s","currency":"BRL"}%s}`,
		c.ext(op.External), w.PlayerID, w.ID, op.Kind, op.Amount, ref)
}

func (c *cluster) submit(api string, w wallet, op operation) response {
	return c.call(api, "POST", "/wagering/transactions", c.token("provider-a"), c.body(w, op), "Idempotency-Key", "provider-a:"+c.ext(op.External))
}

func (c *cluster) send(w wallet, op operation) string {
	c.t.Helper()
	messageID := "msg-" + uuid.NewString()
	data := strings.TrimSuffix(strings.TrimPrefix(c.body(w, op), "{"), "}")
	body := fmt.Sprintf(`{"messageId":"%s","type":"WagerTransactionRequested","occurredAt":"%s","data":{%s,"idempotencyKey":"provider-a:%s"}}`,
		messageID, time.Now().UTC().Format(time.RFC3339), data, c.ext(op.External))
	_, err := c.sqs.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: aws.String(c.queue), MessageBody: aws.String(body),
		MessageGroupId: aws.String(w.ID), MessageDeduplicationId: aws.String(messageID),
	})
	if err != nil {
		c.t.Fatal(err)
	}
	return messageID
}

func (c *cluster) balance(w wallet) string {
	c.t.Helper()
	for i := range c.apis {
		res := c.call(c.api(i), "GET", "/wallets/"+w.ID, c.token("wallet-internal"), "")
		if res.Status == http.StatusOK {
			return res.amount("balance")
		}
	}
	c.t.Fatalf("no instance answered for wallet %s", w.ID)
	return ""
}

func (c *cluster) reconcile(w wallet) response {
	c.t.Helper()
	for i := range c.apis {
		res := c.call(c.api(i), "POST", "/wallets/"+w.ID+"/reconciliation", c.token("wallet-internal"), "")
		if res.Status == http.StatusOK {
			return res
		}
	}
	c.t.Fatalf("no instance answered reconciliation for %s", w.ID)
	return response{}
}

func (c *cluster) status(external string) string {
	for i := range c.apis {
		res := c.call(c.api(i), "GET", "/providers/provider-a/wagering/transactions/"+c.ext(external), c.token("provider-a"), "")
		if res.Status == http.StatusOK {
			return res.str("status")
		}
	}
	return ""
}

// waitAll polls until every external id reaches a terminal state.
func (c *cluster) waitAll(externals []string, timeout time.Duration) (processed, rejected, other int) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		processed, rejected, other = 0, 0, 0
		for _, ext := range externals {
			switch c.status(ext) {
			case "PROCESSED":
				processed++
			case "REJECTED":
				rejected++
			default:
				other++
			}
		}
		if other == 0 || time.Now().After(deadline) {
			return
		}
		time.Sleep(time.Second)
	}
}

func (c *cluster) ledgerDebits(w wallet) int64 {
	c.t.Helper()
	var n int64
	if err := c.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

func (c *cluster) inboxRows(messageIDs []string) int64 {
	c.t.Helper()
	var n int64
	if err := c.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM inbox_messages WHERE message_id = ANY($1)`, messageIDs).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

func (c *cluster) pendingOutbox(w wallet) int64 {
	c.t.Helper()
	var n int64
	err := c.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM outbox_events WHERE published_at IS NULL AND (aggregate_id = $1 OR aggregate_id IN (SELECT id FROM wager_transactions WHERE wallet_id = $1))`, w.ID).Scan(&n)
	if err != nil {
		c.t.Fatal(err)
	}
	return n
}

func (c *cluster) outboxAttempts(w wallet) (events, maxAttempts int64) {
	c.t.Helper()
	err := c.pool.QueryRow(context.Background(), `SELECT COUNT(*), COALESCE(MAX(attempts), 0) FROM outbox_events WHERE aggregate_id = $1 OR aggregate_id IN (SELECT id FROM wager_transactions WHERE wallet_id = $1)`, w.ID).Scan(&events, &maxAttempts)
	if err != nil {
		c.t.Fatal(err)
	}
	return
}
