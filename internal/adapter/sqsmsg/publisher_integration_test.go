//go:build integration

package sqsmsg_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/sqsmsg"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/event"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

func sqsClient(t *testing.T) (*sqs.Client, config.Config) {
	t.Helper()
	if os.Getenv("AWS_ENDPOINT_URL") == "" {
		t.Skip("AWS_ENDPOINT_URL not set")
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		// Only the SQS/AWS part matters here; fill the rest to satisfy Load.
		cfg = config.Config{
			AWS: config.AWS{Region: os.Getenv("AWS_REGION"), Endpoint: os.Getenv("AWS_ENDPOINT_URL")},
			SQS: config.SQS{WagerQueueURL: os.Getenv("SQS_WAGER_QUEUE_URL"), EventsQueueURL: os.Getenv("SQS_EVENTS_QUEUE_URL")},
		}
	}
	client, err := sqsmsg.NewClient(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return client, cfg
}

// drain receives and deletes messages for the given aggregate until the
// queue yields nothing for a short while.
func drain(t *testing.T, client *sqs.Client, queueURL, aggregateID string) []types.Message {
	t.Helper()
	var out []types.Message
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		res, err := client.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
			MessageAttributeNames:       []string{"All"},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range res.Messages {
			if m.Attributes["MessageGroupId"] == aggregateID {
				out = append(out, m)
			}
			_, _ = client.DeleteMessage(t.Context(), &sqs.DeleteMessageInput{QueueUrl: aws.String(queueURL), ReceiptHandle: m.ReceiptHandle})
		}
		if len(res.Messages) == 0 && len(out) > 0 {
			break
		}
	}
	return out
}

// purge empties a queue so earlier runs (load tests, evidence scenarios)
// cannot bury the messages this test looks for.
func purge(t *testing.T, client *sqs.Client, queueURL string) {
	t.Helper()
	if _, err := client.PurgeQueue(t.Context(), &sqs.PurgeQueueInput{QueueUrl: aws.String(queueURL)}); err != nil {
		t.Logf("purge %s: %v (continuing)", queueURL, err)
	}
	time.Sleep(500 * time.Millisecond)
}

func TestPublishDeliversEnvelopeWithFifoAttributes(t *testing.T) {
	client, cfg := sqsClient(t)
	purge(t, client, cfg.SQS.EventsQueueURL)
	pub := sqsmsg.NewEventPublisher(client, cfg.SQS.EventsQueueURL)

	aggregate := uuid.NewString()
	eventID := uuid.NewString()
	amount := `{"amount":"1.00","currency":"BRL"}`
	rec := usecase.OutboxRecord{
		Envelope: event.Envelope{
			EventID: eventID, EventType: event.TypeWalletBalanceChanged, AggregateType: event.AggregateWallet,
			AggregateID: aggregate, CorrelationID: "corr", OccurredAt: time.Now().UTC(), Version: 1,
		},
		Data: json.RawMessage(`{"walletId":"` + aggregate + `","money":` + amount + `}`),
	}
	if errs := pub.Publish(t.Context(), []usecase.OutboxRecord{rec}); errs[0] != nil {
		t.Fatal(errs[0])
	}
	// Same event id again: SQS deduplicates within its window.
	if errs := pub.Publish(t.Context(), []usecase.OutboxRecord{rec}); errs[0] != nil {
		t.Fatal(errs[0])
	}

	msgs := drain(t, client, cfg.SQS.EventsQueueURL, aggregate)
	if len(msgs) != 1 {
		t.Fatalf("received %d messages, want 1 (deduplicated)", len(msgs))
	}
	m := msgs[0]
	if m.Attributes["MessageDeduplicationId"] != eventID || m.Attributes["MessageGroupId"] != aggregate {
		t.Fatalf("fifo attributes: %v", m.Attributes)
	}
	if *m.MessageAttributes["eventType"].StringValue != event.TypeWalletBalanceChanged || *m.MessageAttributes["eventId"].StringValue != eventID {
		t.Fatalf("message attributes: %v", m.MessageAttributes)
	}
	var env struct {
		EventID string          `json:"eventId"`
		Type    string          `json:"eventType"`
		Version int             `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(*m.Body), &env); err != nil {
		t.Fatal(err)
	}
	if env.EventID != eventID || env.Type != event.TypeWalletBalanceChanged || env.Version != 1 || string(env.Data) != string(rec.Data) {
		t.Fatalf("body: %s", *m.Body)
	}
}

func TestReadinessProbesTheQueue(t *testing.T) {
	client, cfg := sqsClient(t)
	if err := sqsmsg.Ready(client, cfg.SQS.WagerQueueURL)(t.Context()); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if err := sqsmsg.Ready(client, cfg.SQS.WagerQueueURL+"-missing")(t.Context()); err == nil {
		t.Fatal("missing queue reported ready")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	if err := sqsmsg.Ready(client, cfg.SQS.WagerQueueURL)(ctx); err == nil {
		t.Fatal("expired context reported ready")
	}
}
