//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/postgres"
	"github.com/fiorellizz/backend-challenge-go/internal/adapter/sqsmsg"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/metrics"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
	"github.com/fiorellizz/backend-challenge-go/internal/worker"
)

func TestSameOperationOverHTTPAndSQS(t *testing.T) {
	s := start(t, nil)
	w := s.openWallet("100.00")
	op := operation{External: "cross-1", Kind: "BET", Amount: "15.00"}

	// The message goes first; HTTP races it while the consumer may or may
	// not have picked it up yet.
	s.send(w, op, "msg-"+uuid.NewString(), uuid.NewString())
	http1 := s.submit(w, op)
	if http1.Status != http.StatusOK || http1.amount("balance") != "85.00" {
		t.Fatalf("http: %d %v", http1.Status, http1.Body)
	}
	final := s.waitStatus("provider-a", "cross-1", 15*time.Second, "PROCESSED")
	if final.amount("balance") != "85.00" {
		t.Fatalf("stored result: %v", final.Body)
	}

	// Give the consumer time to handle the message, then check nothing
	// was applied twice.
	time.Sleep(3 * time.Second)
	if got := s.balance(w); got != "85.00" {
		t.Fatalf("balance %s", got)
	}
	if debits, _ := s.ledgerStats(w); debits != 1 {
		t.Fatalf("debits = %d", debits)
	}
	s.assertConsistent(w)
}

func TestSQSRedeliveryAndDeadLetter(t *testing.T) {
	s := start(t, nil)
	w := s.openWallet("100.00")
	op := operation{External: "sqs-1", Kind: "BET", Amount: "20.00"}
	messageID := "msg-" + uuid.NewString()

	// Two deliveries of the same envelope (distinct dedup ids defeat the
	// broker's own window, as a redelivery after it would).
	s.send(w, op, messageID, uuid.NewString())
	s.send(w, op, messageID, uuid.NewString())
	// A rejection and a poison message.
	s.send(w, operation{External: "sqs-big", Kind: "BET", Amount: "500.00"}, "msg-"+uuid.NewString(), uuid.NewString())
	poison := "msg-" + uuid.NewString()
	_, err := s.sqs.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: aws.String(s.cfg.SQS.WagerQueueURL), MessageBody: aws.String(`{"messageId":"` + poison + `","type":"WagerTransactionRequested","data":{"kind":"OPENING"}}`),
		MessageGroupId: aws.String(w.ID), MessageDeduplicationId: aws.String(uuid.NewString()),
	})
	if err != nil {
		t.Fatal(err)
	}

	s.waitStatus("provider-a", "sqs-1", 15*time.Second, "PROCESSED")
	rejected := s.waitStatus("provider-a", "sqs-big", 15*time.Second, "REJECTED")
	if rejected.str("failureCode") != "INSUFFICIENT_BALANCE" {
		t.Fatalf("rejected via sqs: %v", rejected.Body)
	}
	time.Sleep(2 * time.Second)
	if got := s.balance(w); got != "80.00" {
		t.Fatalf("balance %s", got)
	}
	if n := s.inboxCount(messageID); n != 1 {
		t.Fatalf("inbox rows for %s = %d", messageID, n)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res, err := s.sqs.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(s.cfg.SQS.WagerDLQURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1, MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range res.Messages {
			_, _ = s.sqs.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(s.cfg.SQS.WagerDLQURL), ReceiptHandle: m.ReceiptHandle})
			var env struct {
				MessageID string `json:"messageId"`
			}
			_ = json.Unmarshal([]byte(aws.ToString(m.Body)), &env)
			if env.MessageID == poison {
				if reason := m.MessageAttributes["reason"]; reason.StringValue == nil || *reason.StringValue == "" {
					t.Fatalf("dlq message without reason")
				}
				s.assertConsistent(w)
				return
			}
		}
	}
	t.Fatal("poison message never reached the DLQ")
}

func TestOutboxDeliversEventsWithCompetingPublishers(t *testing.T) {
	s := start(t, nil)
	// Earlier runs (load tests, evidence) leave events on the queue; empty
	// it so the ones produced here can be found in time.
	if _, err := s.sqs.PurgeQueue(context.Background(), &sqs.PurgeQueueInput{QueueUrl: aws.String(s.cfg.SQS.EventsQueueURL)}); err != nil {
		t.Logf("purge events queue: %v (continuing)", err)
	}
	time.Sleep(500 * time.Millisecond)
	w := s.openWallet("100.00")

	// A second publisher, with its own connections, competes with the one
	// inside the application for the same outbox rows.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	uow, reads := postgres.NewUnitOfWork(s.pool), postgres.NewRepositories(s.pool)
	publisher := sqsmsg.NewEventPublisher(s.sqs, s.cfg.SQS.EventsQueueURL)
	svc, err := usecase.NewOutboxService(uow, publisher, usecase.SystemClock, 5, 10, log)
	if err != nil {
		t.Fatal(err)
	}
	second := worker.NewOutboxPublisher(svc, reads, 50*time.Millisecond, metrics.New(), log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { second.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	for i := 0; i < 10; i++ {
		s.submit(w, operation{External: "evt-" + uuid.NewString(), Kind: "BET", Amount: "1.00"})
	}
	s.submit(w, operation{External: "evt-big", Kind: "BET", Amount: "999.00"})

	// Wait until nothing is pending for this wallet's aggregates.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var pending int
		if err := s.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM outbox_events WHERE published_at IS NULL AND aggregate_id = $1`, w.ID).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Every event for this wallet was published exactly once, whichever
	// publisher won the claim.
	rows, err := s.pool.Query(context.Background(),
		`SELECT event_type, attempts, published_at IS NOT NULL FROM outbox_events WHERE aggregate_id = $1 OR aggregate_id IN
		   (SELECT id FROM wager_transactions WHERE wallet_id = $1)`, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var eventType string
		var attempts int
		var published bool
		if err := rows.Scan(&eventType, &attempts, &published); err != nil {
			t.Fatal(err)
		}
		if !published || attempts != 1 {
			t.Fatalf("event %s: published=%v attempts=%d", eventType, published, attempts)
		}
		counts[eventType]++
	}
	// opening + 10 bets processed, 1 rejected; balance changes for opening + 10 bets.
	if counts["WagerTransactionProcessed"] != 11 || counts["WalletBalanceChanged"] != 11 || counts["WagerTransactionRejected"] != 1 {
		t.Fatalf("event counts: %v", counts)
	}

	// The events are on the queue with the right group and dedup ids.
	seen := map[string]int{}
	drainDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(drainDeadline) && len(seen) < 23 {
		res, err := s.sqs.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(s.cfg.SQS.EventsQueueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range res.Messages {
			_, _ = s.sqs.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(s.cfg.SQS.EventsQueueURL), ReceiptHandle: m.ReceiptHandle})
			var env struct {
				EventID     string `json:"eventId"`
				AggregateID string `json:"aggregateId"`
				Data        struct {
					WalletID string `json:"walletId"`
				} `json:"data"`
			}
			_ = json.Unmarshal([]byte(aws.ToString(m.Body)), &env)
			if env.Data.WalletID == w.ID || env.AggregateID == w.ID {
				seen[env.EventID]++
				if m.Attributes["MessageDeduplicationId"] != env.EventID {
					t.Errorf("dedup id %s != eventId %s", m.Attributes["MessageDeduplicationId"], env.EventID)
				}
			}
		}
	}
	if len(seen) != 23 {
		t.Fatalf("received %d distinct events for the wallet, want 23", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("event %s delivered %d times", id, n)
		}
	}
}
