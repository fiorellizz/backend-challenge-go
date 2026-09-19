package sqsmsg

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

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

// sqsBatchLimit is the maximum number of entries SendMessageBatch accepts.
const sqsBatchLimit = 10

// Publish sends the records in SendMessageBatch calls of up to ten. Each
// record gets its own result: a failed entry (or a failed call) is
// reported as transient so the outbox keeps it and retries with backoff,
// while the entries that succeeded are marked published.
func (p *EventPublisher) Publish(ctx context.Context, recs []usecase.OutboxRecord) []error {
	results := make([]error, len(recs))
	// Chunks go out concurrently: ordering inside a FIFO group is already
	// fixed by the sequence numbers the broker assigns per group, and the
	// round trip to the broker dominates the cost of a batch.
	var wg sync.WaitGroup
	for start := 0; start < len(recs); start += sqsBatchLimit {
		end := min(start+sqsBatchLimit, len(recs))
		wg.Add(1)
		go func(chunk []usecase.OutboxRecord, out []error) {
			defer wg.Done()
			p.sendChunk(ctx, chunk, out)
		}(recs[start:end], results[start:end])
	}
	wg.Wait()
	return results
}

func (p *EventPublisher) sendChunk(ctx context.Context, recs []usecase.OutboxRecord, results []error) {
	entries := make([]types.SendMessageBatchRequestEntry, 0, len(recs))
	index := map[string]int{}
	for i, rec := range recs {
		env := rec.Envelope
		env.Data = rec.Data // raw snapshot taken at commit time
		body, err := json.Marshal(env)
		if err != nil {
			results[i] = fmt.Errorf("serialize event %s: %w", env.EventID, err)
			continue
		}
		id := fmt.Sprintf("e%d", i)
		index[id] = i
		entries = append(entries, types.SendMessageBatchRequestEntry{
			Id:                     aws.String(id),
			MessageBody:            aws.String(string(body)),
			MessageGroupId:         aws.String(env.AggregateID),
			MessageDeduplicationId: aws.String(env.EventID),
			MessageAttributes: map[string]types.MessageAttributeValue{
				"eventType": {DataType: aws.String("String"), StringValue: aws.String(env.EventType)},
				"eventId":   {DataType: aws.String("String"), StringValue: aws.String(env.EventID)},
			},
		})
	}
	if len(entries) == 0 {
		return
	}
	out, err := p.client.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{QueueUrl: aws.String(p.queueURL), Entries: entries})
	if err != nil {
		for _, i := range index {
			results[i] = fmt.Errorf("%w: publish batch: %v", errs.ErrTransient, err)
		}
		return
	}
	for _, f := range out.Failed {
		if i, ok := index[aws.ToString(f.Id)]; ok {
			results[i] = fmt.Errorf("%w: publish event %s: %s %s", errs.ErrTransient, recs[i].Envelope.EventID, aws.ToString(f.Code), aws.ToString(f.Message))
		}
	}
}
