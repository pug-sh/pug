package profiles

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

	slog.InfoContext(ctx, "Starting profile worker...")
	return StartWorker(ctx, pgW, natsClient)
}

func StartWorker(ctx context.Context, pgW *pgxpool.Pool, natsClient *natsworker.NATSClient) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("profile-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get profile consumer config: %w", err)
	}

	profileWorker := NewWorker(pgW)

	messageProcessor := func(ctx context.Context, msg jetstream.Msg) error {
		return profileWorker.ProcessMessage(ctx, msg.Data())
	}

	config := natsworker.WorkerConfig{
		StreamName:        consumerConfig.StreamName,
		ConsumerName:      consumerConfig.DurableName,
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
