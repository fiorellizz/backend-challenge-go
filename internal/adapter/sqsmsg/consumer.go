package sqsmsg

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/domain/money"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/metrics"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// ConsumerName identifies this consumer in the inbox table.
const ConsumerName = "wager-transactions-consumer"

// Consumer reads WagerTransactionRequested messages from the FIFO input
// queue and runs them through the same use case as HTTP.
//
// Delivery contract (documented for producers):
//   - MessageGroupId should be the walletId, so operations on one wallet
//     arrive in order; the database serializes them anyway;
//   - MessageDeduplicationId should be the envelope messageId; SQS then
//     drops resends inside its 5-minute window, and the inbox drops them
//     forever after;
//   - the message is deleted only after the handling transaction commits.
//     A crash before that redelivers it; the inbox row written in the same
//     commit makes the redelivery a no-op replay.
//
// Failure handling:
//   - business rejections are committed outcomes: the message is deleted;
//   - malformed messages, unknown wallets and idempotency conflicts are
//     permanent: the message is copied to the DLQ with a reason and deleted;
//   - transient failures (database or broker down) leave the message
//     untouched; it reappears after the visibility timeout and reaches the
//     DLQ through the queue's redrive policy after maxReceiveCount.
type Consumer struct {
	client   *sqs.Client
	cfg      config.SQS
	wagering *usecase.WageringService
	now      usecase.Clock
	metrics  *metrics.Metrics
	log      *slog.Logger
}

// NewConsumer builds the consumer.
func NewConsumer(client *sqs.Client, cfg config.Config, wagering *usecase.WageringService, now usecase.Clock, m *metrics.Metrics, log *slog.Logger) *Consumer {
	return &Consumer{client: client, cfg: cfg.SQS, wagering: wagering, now: now, metrics: m, log: log.With("component", "sqs-consumer")}
}

// requestEnvelope is the input message format.
type requestEnvelope struct {
	MessageID     string      `json:"messageId"`
	Type          string      `json:"type"`
	OccurredAt    time.Time   `json:"occurredAt"`
	CorrelationID string      `json:"correlationId,omitempty"`
	Data          requestData `json:"data"`
}

type requestData struct {
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	IdempotencyKey                 string      `json:"idempotencyKey"`
	PlayerID                       string      `json:"playerId"`
	WalletID                       string      `json:"walletId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	Kind                           string      `json:"kind"`
	Money                          money.Money `json:"money"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
}

const typeWagerTransactionRequested = "WagerTransactionRequested"

// Decision is what Handle concluded about a message.
type Decision int

const (
	// Ack: the outcome is committed; delete the message.
	Ack Decision = iota
	// Reject: the message can never succeed; move it to the DLQ and delete.
	Reject
	// Retry: a transient failure; leave the message for redelivery.
	Retry
)

// Handled reports the decision and, when the use case ran, its result.
type Handled struct {
	Decision  Decision
	Reason    string
	MessageID string
	Result    usecase.ProcessResult
}

// Handle parses one message body and processes it. It never touches SQS.
func (c *Consumer) Handle(ctx context.Context, body string) Handled {
	var env requestEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return Handled{Decision: Reject, Reason: "malformed JSON: " + err.Error()}
	}
	if env.MessageID == "" {
		return Handled{Decision: Reject, Reason: "missing messageId"}
	}
	if env.Type != typeWagerTransactionRequested {
		return Handled{Decision: Reject, Reason: "unsupported type " + env.Type, MessageID: env.MessageID}
	}
	correlation := env.CorrelationID
	if correlation == "" {
		correlation = env.MessageID
	}
	hash := sha256.Sum256([]byte(body))

	start := time.Now()
	result, err := c.wagering.Process(ctx, usecase.ProcessInput{
		IdempotencyKey: env.Data.IdempotencyKey, ProviderID: env.Data.ProviderID,
		ExternalTransactionID: env.Data.ExternalTransactionID, PlayerID: env.Data.PlayerID, WalletID: env.Data.WalletID,
		RoundID: env.Data.RoundID, GameID: env.Data.GameID, Kind: env.Data.Kind, Money: env.Data.Money,
		ReferenceExternalTransactionID: env.Data.ReferenceExternalTransactionID, CorrelationID: correlation,
		Inbox: &usecase.InboxMark{ConsumerName: ConsumerName, MessageID: env.MessageID, PayloadHash: hash[:], ReceivedAt: c.now()},
	})
	switch {
	case err == nil:
		tx := result.Transaction
		c.metrics.ObserveProcessing("sqs", string(tx.Kind()), string(tx.Status()), string(tx.FailureCode()), result.IdempotentReplay, time.Since(start))
		return Handled{Decision: Ack, MessageID: env.MessageID, Result: result}
	case errors.Is(err, errs.ErrConflict):
		c.metrics.ConflictsTotal.WithLabelValues("payload").Inc()
		return Handled{Decision: Reject, Reason: err.Error(), MessageID: env.MessageID}
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound):
		return Handled{Decision: Reject, Reason: err.Error(), MessageID: env.MessageID}
	default:
		return Handled{Decision: Retry, Reason: err.Error(), MessageID: env.MessageID}
	}
}

// Run polls until ctx is cancelled. Cancellation stops the loop before
// the next poll; a poll already in flight is allowed to finish (it lasts
// at most WaitTimeSeconds) and every message it returns is handled to
// completion within the visibility timeout. Aborting the poll instead
// would leave messages the broker had already handed out invisible until
// the visibility timeout expires, which is what a crash does and what a
// graceful stop must not do.
func (c *Consumer) Run(ctx context.Context) {
	c.log.Info("sqs consumer started", "queue", c.cfg.WagerQueueURL)
	defer c.log.Info("sqs consumer stopped")
	for ctx.Err() == nil {
		msgs, err := c.receive(ctx)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("receive failed; will retry", "error", err.Error())
				select {
				case <-ctx.Done():
				case <-time.After(2 * time.Second):
				}
			}
			continue
		}
		for _, m := range msgs {
			c.handleMessage(ctx, m)
		}
	}
}

func (c *Consumer) receive(parent context.Context) ([]types.Message, error) {
	// Detached from cancellation on purpose (see Run); bounded so a broker
	// that stops answering cannot hang the shutdown.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), time.Duration(c.cfg.WaitTimeSeconds+5)*time.Second)
	defer cancel()
	res, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:              aws.String(c.cfg.WagerQueueURL),
		MaxNumberOfMessages:   c.cfg.MaxMessages,
		WaitTimeSeconds:       c.cfg.WaitTimeSeconds,
		VisibilityTimeout:     c.cfg.VisibilityTimeoutSeconds,
		MessageAttributeNames: []string{"All"},
		// MessageGroupId is needed to route rejected messages to the DLQ.
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameMessageGroupId},
	})
	if err != nil {
		return nil, err
	}
	return res.Messages, nil
}

// handleMessage runs Handle with a context detached from the shutdown
// signal but bounded by the visibility timeout, then acts on the decision.
func (c *Consumer) handleMessage(parent context.Context, m types.Message) {
	budget := time.Duration(c.cfg.VisibilityTimeoutSeconds)*time.Second - 2*time.Second
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), budget)
	defer cancel()

	h := c.Handle(ctx, aws.ToString(m.Body))
	log := c.log.With("messageId", h.MessageID, "sqsMessageId", aws.ToString(m.MessageId))
	if h.Result.Transaction != nil {
		log = log.With("transactionId", h.Result.Transaction.ID().String(), "walletId", h.Result.Transaction.WalletID().String(),
			"providerId", h.Result.Transaction.ProviderID(), "status", string(h.Result.Transaction.Status()))
	}

	switch h.Decision {
	case Ack:
		c.metrics.ConsumerMessagesTotal.WithLabelValues("ack").Inc()
		log.Info("message processed", "idempotentReplay", h.Result.IdempotentReplay)
	case Reject:
		c.metrics.ConsumerMessagesTotal.WithLabelValues("reject").Inc()
		log.Warn("message rejected permanently", "reason", h.Reason)
		if err := c.deadLetter(ctx, m, h.Reason); err != nil {
			log.Error("could not move message to dlq; leaving it for redrive", "error", err.Error())
			return
		}
		c.metrics.DLQTotal.Inc()
	case Retry:
		c.metrics.ConsumerMessagesTotal.WithLabelValues("retry").Inc()
		log.Warn("message left for redelivery", "reason", h.Reason)
		return
	}
	if err := c.delete(ctx, m); err != nil {
		// The outcome is committed; a redelivery will be a replay.
		log.Error("could not delete message; redelivery will replay", "error", err.Error())
	}
}

func (c *Consumer) delete(ctx context.Context, m types.Message) error {
	_, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(c.cfg.WagerQueueURL), ReceiptHandle: m.ReceiptHandle})
	return err
}

// deadLetter copies the message to the DLQ with the reason attached. The
// original is deleted afterwards by the caller.
func (c *Consumer) deadLetter(ctx context.Context, m types.Message, reason string) error {
	group := "invalid"
	if g, ok := m.Attributes["MessageGroupId"]; ok && g != "" {
		group = g
	}
	if len(reason) > 256 {
		reason = reason[:256]
	}
	_, err := c.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.cfg.WagerDLQURL),
		MessageBody:            m.Body,
		MessageGroupId:         aws.String(group),
		MessageDeduplicationId: m.MessageId, // the SQS id, unique per delivery
		MessageAttributes: map[string]types.MessageAttributeValue{
			"reason": {DataType: aws.String("String"), StringValue: aws.String(reason)},
		},
	})
	if err != nil {
		return fmt.Errorf("send to dlq: %w", err)
	}
	return nil
}
