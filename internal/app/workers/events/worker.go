package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/fivebitsio/cotton/internal/deps/clickhouse"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
)

func Run(ctx context.Context) error {
	var chCfg clickhouse.Config
	if err := envconfig.Process(ctx, &chCfg); err != nil {
		return err
	}

	chDB, err := clickhouse.NewFromConfig(ctx, &chCfg)
	if err != nil {
		return err
	}
	defer chDB.Close(ctx)

	natsClient, err := natsworker.New(ctx)
	if err != nil {
		return err
	}
	defer natsClient.Close()

	slog.InfoContext(ctx, "Starting events worker...")
	return StartWorker(ctx, chDB.Conn, natsClient)
}

func StartWorker(ctx context.Context, ch driver.Conn, natsClient *natsworker.NATSClient) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("events-writer-durable")
	if err != nil {
		return fmt.Errorf("failed to get events consumer config: %w", err)
	}

	processor := NewProcessor(ch)

	messageProcessor := func(ctx context.Context, msg jetstream.Msg) error {
		err := processor.ProcessMessage(ctx, msg.Data())
		if err != nil && IsPermanentError(err) {
			slog.ErrorContext(ctx, "terminating poison message", slogx.Error(err))
			if termErr := msg.Term(); termErr != nil {
				slog.ErrorContext(ctx, "failed to terminate message", slogx.Error(termErr))
			}
			return nil
		}
		return err
	}

	config := natsworker.WorkerConfig{
		StreamName:        consumerConfig.StreamName,
		ConsumerName:      consumerConfig.DurableName,
		DurableName:       consumerConfig.DurableName,
		Concurrency:       10,
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
