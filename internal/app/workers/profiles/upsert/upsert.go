package upsert

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"buf.build/go/protovalidate"
	"github.com/fivebitsio/cotton/internal/deps/clickhouse"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	workerprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/workers/profiles/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
	"google.golang.org/protobuf/proto"
)

func Run(ctx context.Context) error {
	closeOtel, err := telemetry.SetupSDK(ctx)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := closeOtel(shutdownCtx); err != nil {
			slog.ErrorContext(shutdownCtx, "failed to shutdown telemetry", slogx.Error(err))
		}
	}()

	var chCfg clickhouse.Config
	if err := envconfig.Process(ctx, &chCfg); err != nil {
		return err
	}

	chDB, err := clickhouse.NewFromConfig(ctx, &chCfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := chDB.Close(ctx); err != nil {
			slog.WarnContext(ctx, "failed to close ClickHouse connection", slogx.Error(err))
		}
	}()

	natsClient, err := natsworker.New(ctx)
	if err != nil {
		return err
	}
	defer natsClient.Close()

	slog.InfoContext(ctx, "Starting profile upsert worker...")
	return StartWorker(ctx, chDB.Conn, natsClient)
}

type asyncInserter interface {
	AsyncInsert(ctx context.Context, query string, wait bool, args ...any) error
}

func StartWorker(ctx context.Context, ch asyncInserter, natsClient *natsworker.NATSClient) error {
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

func handleUpsert(ctx context.Context, ch asyncInserter, data []byte) error {
	msg := &workerprofilesv1.ProfileUpsertMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal profile upsert message", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).
			With("worker", "profile-upsert")
	}

	if err := protovalidate.Validate(msg); err != nil {
		slog.ErrorContext(ctx, "upsert message failed validation", slogx.Error(err))
		telemetry.RecordError(ctx, err)
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
			slog.String("profile_id", msg.GetProfileId()))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).
			With("worker", "profile-upsert").
			With("profile_id", msg.GetProfileId())
	}

	var isDeleted uint8
	if msg.GetIsDeleted() {
		isDeleted = 1
	}

	createTime := msg.GetCreateTime().AsTime()
	updateTime := msg.GetUpdateTime().AsTime()

	// Profile upserts are single-row writes. Let ClickHouse buffer them server-side
	// instead of paying a full round trip and part creation cost per message.
	if err := ch.AsyncInsert(ctx,
		"INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)",
		true,
		msg.GetProfileId(), msg.GetProjectId(), msg.GetExternalId(), string(propsJSON), isDeleted, createTime, updateTime,
	); err != nil {
		slog.ErrorContext(ctx, "failed to async insert profile into ClickHouse", slogx.Error(err),
			slog.String("profile_id", msg.GetProfileId()))
		telemetry.RecordError(ctx, err)
		return fmt.Errorf("async insert profile: %w", err)
	}

	return nil
}
