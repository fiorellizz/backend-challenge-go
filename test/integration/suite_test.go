//go:build integration

// Package integration exercises the whole service against real
// PostgreSQL, LocalStack (SQS) and Keycloak. The application is started in
// process with the same Fx modules main uses; multi-process scenarios live
// in test/evidence. Run with `make test-integration` after `make infra`.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/httpapi"
	"github.com/fiorellizz/backend-challenge-go/internal/adapter/sqsmsg"
	"github.com/fiorellizz/backend-challenge-go/internal/fxmodules"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

// stack is one running application plus the clients the tests use.
type stack struct {
	t      *testing.T
	app    *fxtest.App
	cfg    config.Config
	base   string
	pool   *pgxpool.Pool
	sqs    *sqs.Client
	mu     sync.Mutex
	tokens map[string]string
	// run makes external transaction ids unique per test run: idempotency
	// keys are global, so fixed ids would collide with earlier runs.
	run string
}

// ext namespaces an external transaction id to this run.
func (s *stack) ext(name string) string { return s.run + "-" + name }

// testConfig builds the configuration from the environment (see the
// Makefile's INTEGRATION_ENV) with test-friendly overrides applied.
func testConfig(t *testing.T, overrides map[string]string) config.Config {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" || os.Getenv("AWS_ENDPOINT_URL") == "" || os.Getenv("OIDC_ISSUER_URL") == "" {
		t.Skip("DATABASE_URL, AWS_ENDPOINT_URL and OIDC_ISSUER_URL are required")
	}
	logLevel := os.Getenv("TEST_LOG_LEVEL")
	if logLevel == "" {
		logLevel = "warn"
	}
	defaults := map[string]string{
		"APP_INSTANCE_ID":                "integration",
		"HTTP_ADDR":                      "127.0.0.1:0",
		"LOG_LEVEL":                      logLevel,
		"SQS_WAIT_TIME_SECONDS":          "1",
		"SQS_VISIBILITY_TIMEOUT_SECONDS": "5",
		"OUTBOX_POLL_INTERVAL":           "200ms",
		"REFERENCE_POLL_INTERVAL":        "200ms",
		"REFERENCE_BASE_BACKOFF":         "500ms",
		"REFERENCE_MAX_ATTEMPTS":         "8",
		"REFERENCE_TTL":                  "1m",
		"SHUTDOWN_TIMEOUT":               "15s",
	}
	for k, v := range overrides {
		defaults[k] = v
	}
	cfg, err := config.Load(func(k string) string {
		if v, ok := defaults[k]; ok {
			return v
		}
		return os.Getenv(k)
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// start boots the application and waits until it reports ready.
func start(t *testing.T, overrides map[string]string) *stack {
	t.Helper()
	cfg := testConfig(t, overrides)
	var (
		srv    *httpapi.Server
		pool   *pgxpool.Pool
		client *sqs.Client
	)
	app := fxtest.New(t, append(fxmodules.App(cfg), fx.Populate(&srv, &pool, &client))...)
	app.RequireStart()
	s := &stack{t: t, app: app, cfg: cfg, base: "http://" + srv.Addr(), pool: pool, sqs: client,
		tokens: map[string]string{}, run: uuid.NewString()[:8]}
	t.Cleanup(func() {
		if s.app != nil {
			s.stop()
		}
	})
	s.waitReady()
	return s
}

// stop shuts the application down and asserts it did so within budget.
func (s *stack) stop() {
	s.t.Helper()
	started := time.Now()
	s.app.RequireStop()
	if elapsed := time.Since(started); elapsed > s.cfg.ShutdownTimeout {
		s.t.Fatalf("shutdown took %s, more than %s", elapsed, s.cfg.ShutdownTimeout)
	}
	s.app = nil
}

func (s *stack) waitReady() {
	s.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(s.base + "/health/ready")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatal("application never became ready")
}

// token obtains a client_credentials access token from Keycloak, cached
// per client for the test's lifetime.
func (s *stack) token(clientID string) string {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok, ok := s.tokens[clientID]; ok {
		return tok
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientID + "-secret"}}
	res, err := http.PostForm(s.cfg.OIDC.IssuerURL+"/protocol/openid-connect/token", form)
	if err != nil {
		s.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if res.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil || out.AccessToken == "" {
		s.t.Fatalf("token for %s: %d %s", clientID, res.StatusCode, body)
	}
	s.tokens[clientID] = out.AccessToken
	return out.AccessToken
}

// response is a decoded HTTP reply.
type response struct {
	Status int
	Body   map[string]any
}

func (r response) str(key string) string {
	v, _ := r.Body[key].(string)
	return v
}

func (r response) amount(key string) string {
	m, _ := r.Body[key].(map[string]any)
	v, _ := m["amount"].(string)
	return v
}

func (r response) errorCode() string {
	e, _ := r.Body["error"].(map[string]any)
	v, _ := e["code"].(string)
	return v
}

// call performs one request. token may be empty for anonymous calls.
func (s *stack) call(method, path, token, body string, headers ...string) response {
	s.t.Helper()
	req, err := http.NewRequest(method, s.base+path, bytes.NewBufferString(body))
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := response{Status: res.StatusCode}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.Body); err != nil {
			s.t.Fatalf("%s %s: non-JSON body (%d): %s", method, path, res.StatusCode, raw)
		}
	}
	return out
}

// wallet is a freshly opened wallet with its owner.
type wallet struct {
	ID       string
	PlayerID string
}

func (s *stack) openWallet(amount string) wallet {
	s.t.Helper()
	player := uuid.NewString()
	res := s.call("POST", "/wallets", s.token("wallet-internal"),
		fmt.Sprintf(`{"playerId":"%s","initialBalance":{"amount":"%s","currency":"BRL"}}`, player, amount))
	if res.Status != http.StatusCreated {
		s.t.Fatalf("open wallet: %d %v", res.Status, res.Body)
	}
	return wallet{ID: res.str("id"), PlayerID: player}
}

// operation describes a provider request; zero values take defaults.
type operation struct {
	Provider  string
	External  string
	Kind      string
	Amount    string
	Reference string
	Round     string
}

func (op operation) body(w wallet) string {
	if op.Provider == "" {
		op.Provider = "provider-a"
	}
	if op.Round == "" {
		op.Round = "round-1"
	}
	ref := ""
	if op.Reference != "" {
		ref = fmt.Sprintf(`,"referenceExternalTransactionId":"%s"`, op.Reference)
	}
	return fmt.Sprintf(`{"providerId":"%s","externalTransactionId":"%s","playerId":"%s","walletId":"%s","roundId":"%s","gameId":"fortune-chimp","kind":"%s","money":{"amount":"%s","currency":"BRL"}%s}`,
		op.Provider, op.External, w.PlayerID, w.ID, op.Round, op.Kind, op.Amount, ref)
}

func (op operation) key() string {
	if op.Provider == "" {
		op.Provider = "provider-a"
	}
	return op.Provider + ":" + op.External
}

func (op operation) providerClient() string {
	if op.Provider == "" {
		return "provider-a"
	}
	return op.Provider
}

// scoped returns the operation with its ids namespaced to this run.
func (s *stack) scoped(op operation) operation {
	op.External = s.ext(op.External)
	if op.Reference != "" {
		op.Reference = s.ext(op.Reference)
	}
	return op
}

// submit sends the operation over HTTP with the provider's token.
func (s *stack) submit(w wallet, op operation) response {
	s.t.Helper()
	op = s.scoped(op)
	return s.call("POST", "/wagering/transactions", s.token(op.providerClient()), op.body(w), "Idempotency-Key", op.key())
}

// envelope renders the SQS message for the operation.
func (op operation) envelope(w wallet, messageID string) string {
	body := op.body(w)
	data := strings.TrimSuffix(strings.TrimPrefix(body, "{"), "}")
	return fmt.Sprintf(`{"messageId":"%s","type":"WagerTransactionRequested","occurredAt":"%s","data":{%s,"idempotencyKey":"%s"}}`,
		messageID, time.Now().UTC().Format(time.RFC3339), data, op.key())
}

// send publishes the operation to the input queue as a provider would.
func (s *stack) send(w wallet, op operation, messageID, dedupID string) {
	s.t.Helper()
	op = s.scoped(op)
	_, err := s.sqs.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: aws.String(s.cfg.SQS.WagerQueueURL), MessageBody: aws.String(op.envelope(w, messageID)),
		MessageGroupId: aws.String(w.ID), MessageDeduplicationId: aws.String(dedupID),
	})
	if err != nil {
		s.t.Fatal(err)
	}
}

func (s *stack) getWallet(w wallet) response {
	s.t.Helper()
	return s.call("GET", "/wallets/"+w.ID, s.token("wallet-internal"), "")
}

func (s *stack) balance(w wallet) string {
	s.t.Helper()
	res := s.getWallet(w)
	if res.Status != http.StatusOK {
		s.t.Fatalf("get wallet: %d %v", res.Status, res.Body)
	}
	return res.amount("balance")
}

func (s *stack) reconcile(w wallet) response {
	s.t.Helper()
	return s.call("POST", "/wallets/"+w.ID+"/reconciliation", s.token("wallet-internal"), "")
}

func (s *stack) providerTransaction(provider, external string) response {
	s.t.Helper()
	return s.call("GET", "/providers/"+provider+"/wagering/transactions/"+s.ext(external), s.token(provider), "")
}

// waitStatus polls the provider query until the transaction reaches one
// of the statuses or the deadline passes.
func (s *stack) waitStatus(provider, external string, timeout time.Duration, statuses ...string) response {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	var last response
	for time.Now().Before(deadline) {
		last = s.providerTransaction(provider, external)
		for _, st := range statuses {
			if last.Status == http.StatusOK && last.str("status") == st {
				return last
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	s.t.Fatalf("transaction %s did not reach %v: %d %v", external, statuses, last.Status, last.Body)
	return last
}

// ledgerStats reads the ledger straight from the database.
func (s *stack) ledgerStats(w wallet) (debits, credits int64) {
	s.t.Helper()
	err := s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FILTER (WHERE direction = 'DEBIT'), COUNT(*) FILTER (WHERE direction = 'CREDIT')
		   FROM wallet_ledger_entries WHERE wallet_id = $1`, w.ID).Scan(&debits, &credits)
	if err != nil {
		s.t.Fatal(err)
	}
	return debits, credits
}

func (s *stack) inboxCount(messageID string) int64 {
	s.t.Helper()
	var n int64
	if err := s.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM inbox_messages WHERE consumer_name = $1 AND message_id = $2`, sqsmsg.ConsumerName, messageID).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// assertConsistent checks the stored balance against the ledger.
func (s *stack) assertConsistent(w wallet) {
	s.t.Helper()
	res := s.reconcile(w)
	if res.Status != http.StatusOK || res.Body["consistent"] != true {
		s.t.Fatalf("reconciliation: %d %v", res.Status, res.Body)
	}
}
