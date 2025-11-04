package subscriptions

import (
	"context"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	pulsarworker "github.com/fivebitsio/cotton/pkg/pulsar"
	"github.com/jackc/pgx/v5/pgxpool"
)

func StartConsumer(ctx context.Context, pgRO *pgxpool.Pool, pgW *pgxpool.Pool, pulsarClient *pulsarworker.Client) error {
	subscriptionConsumer := NewConsumer(pgRO, pgW)

	messageProcessor := func(ctx context.Context, msg pulsar.Message) error {
		return subscriptionConsumer.ProcessMessage(ctx, msg.Payload())
	}

	config := pulsarworker.WorkerConfig{
		ConsumerOptions: pulsar.ConsumerOptions{
			Topic:            "persistent://public/default/subscriptions",
			SubscriptionName: "subscriptions-processor",
			Type:             pulsar.Shared,
		},
		Concurrency:       100,
		ProcessingTimeout: 30 * time.Second,
	}

	worker, err := pulsarworker.NewWorker(config, messageProcessor)
	if err != nil {
		return err
	}

	return worker.Start(ctx, pulsarClient)
}
