package fxmodules

import (
	"context"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/httpapi"
	"github.com/fiorellizz/backend-challenge-go/internal/adapter/sqsmsg"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/usecase"
)

// SQS provides the broker client, the event publisher and the readiness
// check. The client is stateless, so it has no lifecycle of its own.
var SQS = fx.Module("sqs",
	fx.Provide(
		newSQSClient,
		newEventPublisher,
		sqsmsg.NewConsumer,
		fx.Annotate(sqsReadiness, fx.ResultTags(`group:"readiness"`)),
	),
)

func newSQSClient(cfg config.Config) (*sqs.Client, error) {
	return sqsmsg.NewClient(context.Background(), cfg)
}

func newEventPublisher(cfg config.Config, client *sqs.Client) usecase.EventPublisher {
	return sqsmsg.NewEventPublisher(client, cfg.SQS.EventsQueueURL)
}

func sqsReadiness(cfg config.Config, client *sqs.Client) httpapi.ReadinessCheck {
	return httpapi.ReadinessCheck{Name: "sqs", Check: sqsmsg.Ready(client, cfg.SQS.WagerQueueURL)}
}

func newOutboxService(cfg config.Config, uow usecase.UnitOfWork, publisher usecase.EventPublisher, now usecase.Clock, log *slog.Logger) (*usecase.OutboxService, error) {
	return usecase.NewOutboxService(uow, publisher, now, cfg.Outbox.BatchSize, cfg.Outbox.MaxAttempts, log)
}
