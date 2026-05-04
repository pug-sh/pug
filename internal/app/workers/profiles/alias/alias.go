package alias

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"buf.build/go/protovalidate"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/pug-sh/pug/internal/deps/clickhouse"
	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	workerprofilesv1 "github.com/pug-sh/pug/internal/gen/proto/workers/profiles/v1"
	"github.com/pug-sh/pug/internal/slogx"
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

	slog.InfoContext(ctx, "Starting profile alias worker...")
	return StartWorker(ctx, chDB.Conn, natsClient)
}

func StartWorker(ctx context.Context, ch driver.Conn, natsClient *natsworker.NATSClient) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("profile-alias-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get profile alias consumer config: %w", err)
	}

	messageProcessor := func(ctx context.Context, msg jetstream.Msg) error {
		return handleAlias(ctx, ch, msg.Data())
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
		DLQSubject:        natsworker.DLQProfilesAliasSubject,
	}

	worker, err := natsworker.NewWorker(config, messageProcessor, natsClient)
	if err != nil {
		return err
	}

	return worker.Start(ctx)
}

func handleAlias(ctx context.Context, ch driver.Conn, data []byte) error {
	msg := &workerprofilesv1.ProfileAliasMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal alias message", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).
			With("worker", "profile-alias")
	}

	if err := protovalidate.Validate(msg); err != nil {
		slog.ErrorContext(ctx, "alias message failed validation", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).
			With("worker", "profile-alias")
	}

	aliasID := msg.GetAliasId()
	profileID := msg.GetProfileId()
	externalID := msg.GetExternalId()
	projectID := msg.GetProjectId()

	if err := ch.Exec(ctx,
		"INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)",
		aliasID, profileID, externalID, projectID,
	); err != nil {
		slog.ErrorContext(ctx, "failed inserting profile alias into ClickHouse", slogx.Error(err),
			slog.String("alias_id", aliasID), slog.String("profile_id", profileID))
		telemetry.RecordError(ctx, err)
		return err
	}

	return nil
}
