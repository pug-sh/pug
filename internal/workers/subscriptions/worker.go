package subscriptions

import (
	"context"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	pulsarworker "github.com/fivebitsio/cotton/pkg/pulsar"
	"github.com/jackc/pgx/v5/pgxpool"
)

func StartWorker(ctx context.Context, pgRO *pgxpool.Pool, pgW *pgxpool.Pool, pulsarClient *pulsarworker.Client) error {
	subscriptionWorker := NewWorker(pgRO, pgW)

	messageProcessor := func(ctx context.Context, msg pulsar.Message) error {
		return subscriptionWorker.ProcessMessage(ctx, msg.Payload())
	}

	config := pulsarworker.WorkerConfig{
		WorkerOptions: pulsar.ConsumerOptions{
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
