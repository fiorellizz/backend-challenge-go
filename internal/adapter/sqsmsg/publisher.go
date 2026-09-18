package sqsmsg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/fiorellizz/backend-challenge-go/internal/domain/errs"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// EventPublisher sends outbox records to the events FIFO queue.
//
// Routing contract for consumers of wager-events.fifo:
//   - body: the event envelope as JSON (eventId, eventType, aggregateType,
//     aggregateId, correlationId, causationId, occurredAt, version, data);
//   - MessageGroupId = aggregateId, so events of one wallet or transaction
//     are delivered in order;
//   - MessageDeduplicationId = eventId, so a republication after a crash is
//     dropped by SQS inside its 5-minute window; beyond it, consumers must
//     deduplicate on eventId themselves;
//   - message attributes eventType and eventId, for filtering without
//     parsing the body.
type EventPublisher struct {
	client   *sqs.Client
	queueURL string
}

// NewEventPublisher binds the publisher to the events queue.
func NewEventPublisher(client *sqs.Client, queueURL string) *EventPublisher {
	return &EventPublisher{client: client, queueURL: queueURL}
}

// Publish sends one record. Any SQS failure is reported as transient: the
// outbox keeps the event and retries with backoff.
func (p *EventPublisher) Publish(ctx context.Context, rec usecase.OutboxRecord) error {
	env := rec.Envelope
	env.Data = rec.Data // raw snapshot taken at commit time
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("serialize event %s: %w", env.EventID, err)
	}
	_, err = p.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(p.queueURL),
		MessageBody:            aws.String(string(body)),
		MessageGroupId:         aws.String(env.AggregateID),
		MessageDeduplicationId: aws.String(env.EventID),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"eventType": {DataType: aws.String("String"), StringValue: aws.String(env.EventType)},
			"eventId":   {DataType: aws.String("String"), StringValue: aws.String(env.EventID)},
		},
	})
	if err != nil {
		return fmt.Errorf("%w: publish event %s: %v", errs.ErrTransient, env.EventID, err)
	}
	return nil
}
