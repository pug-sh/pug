package upsert

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/fivebitsio/cotton/internal/deps/clickhouse"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	workerprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/workers/profiles/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
	"google.golang.org/protobuf/proto"
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
	defer func() { _ = chDB.Close(ctx) }()

	natsClient, err := natsworker.New(ctx)
	if err != nil {
		return err
	}
	defer natsClient.Close()

	slog.InfoContext(ctx, "Starting profile upsert worker...")
	return StartWorker(ctx, chDB.Conn, natsClient)
}

func StartWorker(ctx context.Context, ch driver.Conn, natsClient *natsworker.NATSClient) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("profile-upsert-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get profile upsert consumer config: %w", err)
	}

	messageProcessor := func(ctx context.Context, msg jetstream.Msg) error {
		return handleUpsert(ctx, ch, msg.Data())
	}

	config := natsworker.WorkerConfig{
		StreamName:        consumerConfig.StreamName,
		ConsumerName:      consumerConfig.DurableName,
		DurableName:       consumerConfig.DurableName,
		FilterSubject:     consumerConfig.FilterSubject,
		Concurrency:       100,
		ProcessingTimeout: 25 * time.Second,
		MaxDeliver:        consumerConfig.MaxDeliver,
		AckWait:           30 * time.Second,
		DLQSubject:        natsworker.DLQProfilesUpsertSubject,
	}

	worker, err := natsworker.NewWorker(config, messageProcessor, natsClient)
	if err != nil {
		return err
	}

	return worker.Start(ctx)
}

func handleUpsert(ctx context.Context, ch driver.Conn, data []byte) error {
	msg := &workerprofilesv1.ProfileUpsertMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal profile upsert message", slogx.Error(err))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-upsert")
	}

	props := msg.GetProperties().AsMap()
	if props == nil {
		props = map[string]any{}
	}

	propsJSON, err := json.Marshal(props)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal profile properties", slogx.Error(err),
			slog.String("profileId", msg.GetProfileId()))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-upsert").
			With("profile_id", msg.GetProfileId())
	}

	var isDeleted uint8
	if msg.GetIsDeleted() {
		isDeleted = 1
	}

	batch, err := ch.PrepareBatch(ctx, "INSERT INTO profiles (id, project_id, external_id, properties, is_deleted)")
	if err != nil {
		slog.ErrorContext(ctx, "failed to prepare ClickHouse batch", slogx.Error(err),
			slog.String("profileId", msg.GetProfileId()))
		return err
	}

	sent := false
	defer func() {
		if !sent {
			if err := batch.Abort(); err != nil {
				slog.ErrorContext(ctx, "failed to abort ClickHouse batch", slogx.Error(err),
					slog.String("profileId", msg.GetProfileId()))
			}
		}
	}()

	if err := batch.Append(msg.GetProfileId(), msg.GetProjectId(), msg.GetExternalId(), string(propsJSON), isDeleted); err != nil {
		slog.ErrorContext(ctx, "failed to append profile to batch", slogx.Error(err),
			slog.String("profileId", msg.GetProfileId()))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-upsert").
			With("profile_id", msg.GetProfileId())
	}

	if err := batch.Send(); err != nil {
		slog.ErrorContext(ctx, "failed to send profile batch to ClickHouse", slogx.Error(err),
			slog.String("profileId", msg.GetProfileId()))
		return err
	}
	sent = true

	return nil
}
