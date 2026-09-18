//go:build integration

package sqsmsg_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/postgres"
	"github.com/fiorellizz/backend-challenge-go/internal/adapter/sqsmsg"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/id"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/wagering"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

type realStack struct {
	client   *sqs.Client
	cfg      config.Config
	consumer *sqsmsg.Consumer
	wallets  *usecase.WalletService
	wagering *usecase.WageringService
	walletID string
	playerID string
}

func newRealStack(t *testing.T) *realStack {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" || os.Getenv("AWS_ENDPOINT_URL") == "" {
		t.Skip("DATABASE_URL and AWS_ENDPOINT_URL required")
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SQS.WaitTimeSeconds = 1
	pool, err := postgres.NewPool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	client, err := sqsmsg.NewClient(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	uow, reads := postgres.NewUnitOfWork(pool), postgres.NewRepositories(pool)
	wallets := usecase.NewWalletService(uow, reads, usecase.SystemClock)
	svc, err := usecase.NewWageringService(uow, reads, usecase.SystemClock, wagering.ReferencePolicy{BaseBackoff: time.Second, MaxAttempts: 3, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	playerID := uuid.NewString()
	initial, _ := money.Parse("100.00", "BRL")
	w, err := wallets.Open(t.Context(), usecase.OpenWalletInput{PlayerID: id.PlayerID(playerID), InitialBalance: initial})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &realStack{
		client: client, cfg: cfg, consumer: sqsmsg.NewConsumer(client, cfg, svc, usecase.SystemClock, log),
		wallets: wallets, wagering: svc, walletID: w.ID().String(), playerID: playerID,
	}
}

func (s *realStack) send(t *testing.T, body, dedup string) {
	t.Helper()
	_, err := s.client.SendMessage(t.Context(), &sqs.SendMessageInput{
		QueueUrl: aws.String(s.cfg.SQS.WagerQueueURL), MessageBody: aws.String(body),
		MessageGroupId: aws.String(s.walletID), MessageDeduplicationId: aws.String(dedup),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (s *realStack) envelope(messageID, external, amount string) string {
	return fmt.Sprintf(`{"messageId":"%s","type":"WagerTransactionRequested","occurredAt":"2026-09-18T12:00:00Z","data":{"providerId":"provider-a","externalTransactionId":"%s","idempotencyKey":"provider-a:%s","playerId":"%s","walletId":"%s","roundId":"r","gameId":"g","kind":"BET","money":{"amount":"%s","currency":"BRL"}}}`,
		messageID, external, external, s.playerID, s.walletID, amount)
}

// runFor runs the consumer loop for d, then returns once it stopped.
func (s *realStack) runFor(t *testing.T, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	done := make(chan struct{})
	go func() { s.consumer.Run(ctx); close(done) }()
	<-done
}

func (s *realStack) balance(t *testing.T) string {
	t.Helper()
	w, err := s.wallets.Get(t.Context(), id.WalletID(s.walletID))
	if err != nil {
		t.Fatal(err)
	}
	return w.Balance().Amount()
}

func (s *realStack) dlqMessages(t *testing.T) []types.Message {
	t.Helper()
	var out []types.Message
	for i := 0; i < 3; i++ {
		res, err := s.client.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(s.cfg.SQS.WagerDLQURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range res.Messages {
			out = append(out, m)
			_, _ = s.client.DeleteMessage(t.Context(), &sqs.DeleteMessageInput{QueueUrl: aws.String(s.cfg.SQS.WagerDLQURL), ReceiptHandle: m.ReceiptHandle})
		}
	}
	return out
}

func TestConsumerProcessesRedeliversAndDeadLetters(t *testing.T) {
	s := newRealStack(t)
	external := "tx-" + uuid.NewString()
	messageID := "msg-" + uuid.NewString()

	// A valid operation, the same envelope forced through again (SQS would
	// normally deduplicate; a distinct dedup id simulates a redelivery
	// after the window), and a malformed message.
	s.send(t, s.envelope(messageID, external, "25.00"), uuid.NewString())
	s.send(t, s.envelope(messageID, external, "25.00"), uuid.NewString())
	poisonID := "msg-" + uuid.NewString()
	s.send(t, `{"messageId":"`+poisonID+`","type":"WagerTransactionRequested","data":{"kind":"OPENING"}}`, uuid.NewString())

	s.runFor(t, 6*time.Second)

	if got := s.balance(t); got != "75.00" {
		t.Fatalf("balance %s: the redelivery must not debit twice", got)
	}
	tx, err := s.wagering.GetProviderTransaction(t.Context(), "provider-a", external)
	if err != nil || tx.Status() != wagering.Processed {
		t.Fatalf("transaction: %v %v", err, tx)
	}

	attrs, err := s.client.GetQueueAttributes(t.Context(), &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(s.cfg.SQS.WagerQueueURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages},
	})
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Attributes["ApproximateNumberOfMessages"] != "0" {
		t.Fatalf("input queue still has %s messages", attrs.Attributes["ApproximateNumberOfMessages"])
	}

	found := false
	for _, m := range s.dlqMessages(t) {
		if reason, ok := m.MessageAttributes["reason"]; ok && aws.ToString(m.Body) != "" && strings.Contains(aws.ToString(m.Body), poisonID) {
			found = true
			if !strings.Contains(aws.ToString(reason.StringValue), "validation") {
				t.Errorf("dlq reason = %q", aws.ToString(reason.StringValue))
			}
		}
	}
	if !found {
		t.Fatalf("poison message not found in the DLQ")
	}
}
