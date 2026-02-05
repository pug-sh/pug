package campaigns

import (
	"context"
	"fmt"
	"time"

	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
)

func StartWorker(ctx context.Context, pgRO *pgxpool.Pool, pgW *pgxpool.Pool, natsClient *natsworker.NATSClient) error {
	// Get consumer configuration from YAML file
	consumerConfig, err := natsClient.GetConsumerConfigByName("campaign-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get campaign consumer config: %w", err)
	}

	campaignWorker := NewWorker(pgRO, pgW)

	messageProcessor := func(ctx context.Context, msg jetstream.Msg) error {
		return campaignWorker.ProcessMessage(ctx, msg.Data())
	}

	config := natsworker.WorkerConfig{
		StreamName:        consumerConfig.StreamName,
		ConsumerName:      consumerConfig.DurableName, // Use the durable name for both
		DurableName:       consumerConfig.DurableName,
		Concurrency:       100,
		ProcessingTimeout: 30 * time.Second,
		MaxDeliver:        consumerConfig.MaxDeliver,
		AckWait:           30 * time.Second,
	}

	worker, err := natsworker.NewWorker(config, messageProcessor)
	if err != nil {
		return err
	}

	return worker.Start(ctx, natsClient)
}
