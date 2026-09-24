package alias

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"buf.build/go/protovalidate"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/pug-sh/pug/internal/core/deletion"
	"github.com/pug-sh/pug/internal/deps/clickhouse"
	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
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
	defer telemetry.ShutdownOnExit(ctx, closeOtel)

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
	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return err
	}
	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return err
	}
	defer pgW.Close()

	natsClient, err := natsworker.New(ctx)
	if err != nil {
		return err
	}
	defer natsClient.Close()

	slog.InfoContext(ctx, "Starting profile alias worker...")
	return startWorker(ctx, chDB.Conn, natsClient, deletion.NewGate(pgW))
}

func StartWorker(ctx context.Context, ch driver.Conn, natsClient *natsworker.NATSClient) error {
	return startWorker(ctx, ch, natsClient, nil)
}

func startWorker(ctx context.Context, ch driver.Conn, natsClient *natsworker.NATSClient, gate *deletion.Gate) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("profile-alias-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get profile alias consumer config: %w", err)
	}

	messageProcessor := func(ctx context.Context, msg jetstream.Msg) error {
		return handleAlias(ctx, ch, msg.Data(), gate)
	}

	config := natsworker.WorkerConfig{
		StreamName:        consumerConfig.StreamName,
		ConsumerName:      consumerConfig.DurableName,
		DurableName:       consumerConfig.DurableName,
		FilterSubject:     consumerConfig.FilterSubject,
		Concurrency:       gate.ConcurrencyLimit(100),
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

func handleAlias(ctx context.Context, ch driver.Conn, data []byte, gates ...*deletion.Gate) error {
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
	if len(gates) > 0 && gates[0] != nil {
		err := gates[0].WithActiveProjectExternal(ctx, projectID, func(ctx context.Context) error {
			return insertAlias(ctx, ch, aliasID, profileID, externalID, projectID)
		})
		if errors.Is(err, deletion.ErrProjectInactive) {
			return nil
		}
		return err
	}
	return insertAlias(ctx, ch, aliasID, profileID, externalID, projectID)
}

func insertAlias(ctx context.Context, ch driver.Conn, aliasID, profileID, externalID, projectID string) error {
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
