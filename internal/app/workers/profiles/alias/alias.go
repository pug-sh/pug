package alias

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/fivebitsio/cotton/internal/deps/clickhouse"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
	"google.golang.org/protobuf/proto"

	profilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/profiles/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
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
	}

	worker, err := natsworker.NewWorker(config, messageProcessor)
	if err != nil {
		return err
	}

	return worker.Start(ctx, natsClient)
}

func handleAlias(ctx context.Context, ch driver.Conn, data []byte) error {
	msg := &profilesv1.ProfileAliasMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal alias message", slogx.Error(err))
		return &natsworker.PermanentError{Err: err}
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
			slog.String("aliasId", aliasID), slog.String("profileId", profileID))
		return err
	}

	return nil
}
