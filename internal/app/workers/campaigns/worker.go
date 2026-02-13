package campaigns

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
)

func Run(ctx context.Context) error {
	var cfg postgres.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return err
	}

	pgRO, err := postgres.NewReaderPool(ctx, &cfg)
	if err != nil {
		return err
	}
	defer pgRO.Close()

	pgW, err := postgres.NewWriterPool(ctx, &cfg)
	if err != nil {
		return err
	}
	defer pgW.Close()

	natsClient, err := natsworker.New(ctx)
	if err != nil {
		return err
	}
	defer natsClient.Close()

	slog.InfoContext(ctx, "Starting campaign worker...")
	return StartWorker(ctx, pgRO, pgW, natsClient)
}

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
		ConsumerName:      consumerConfig.DurableName,
		DurableName:       consumerConfig.DurableName,
		Concurrency:       100,
		ProcessingTimeout: 25 * time.Second,
		MaxDeliver:        consumerConfig.MaxDeliver,
		AckWait:           30 * time.Second,
		DLQSubject:        natsworker.DLQCampaignsSubject,
	}

	worker, err := natsworker.NewWorker(config, messageProcessor, natsClient)
	if err != nil {
		return err
	}

	return worker.Start(ctx)
}
