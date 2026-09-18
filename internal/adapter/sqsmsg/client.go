// Package sqsmsg talks to AWS SQS (LocalStack locally): it publishes
// integration events from the outbox and consumes provider operations
// from the FIFO input queue.
package sqsmsg

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

// NewClient builds the SQS client. Credentials come from the standard AWS
// environment variables; AWS_ENDPOINT_URL points it at LocalStack.
func NewClient(ctx context.Context, cfg config.Config) (*sqs.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWS.Region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.AWS.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.AWS.Endpoint)
		}
	}), nil
}

// Ready probes the input queue's attributes: cheap, and it fails when the
// broker is down or the queue was not provisioned.
func Ready(client *sqs.Client, queueURL string) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl:       aws.String(queueURL),
			AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
		})
		if err != nil {
			return fmt.Errorf("sqs: %w", err)
		}
		return nil
	}
}
