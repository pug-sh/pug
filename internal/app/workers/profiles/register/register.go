package register

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"errors"

	"github.com/fivebitsio/cotton/internal/app/workers/profiles"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
	"google.golang.org/protobuf/proto"

	sdkprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
)

func Run(ctx context.Context) error {
	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return err
	}

	pgRO, err := postgres.NewReaderPool(ctx, &pgCfg)
	if err != nil {
		return err
	}
	defer pgRO.Close()

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

	slog.InfoContext(ctx, "Starting profile register worker...")
	return StartWorker(ctx, pgRO, pgW, natsClient)
}

func StartWorker(ctx context.Context, pgRO, pgW *pgxpool.Pool, natsClient *natsworker.NATSClient) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("profile-register-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get profile register consumer config: %w", err)
	}

	profileWorker := profiles.NewWorker(pgRO, pgW)

	messageProcessor := func(ctx context.Context, msg jetstream.Msg) error {
		return handleRegister(ctx, profileWorker, msg.Data())
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
		DLQSubject:        natsworker.DLQProfilesRegisterSubject,
	}

	worker, err := natsworker.NewWorker(config, messageProcessor, natsClient)
	if err != nil {
		return err
	}

	return worker.Start(ctx)
}

func handleRegister(ctx context.Context, w *profiles.Worker, data []byte) error {
	msg := &sdkprofilesv1.ProfileRegisterMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal register message", slogx.Error(err))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-register")
	}

	props := msg.GetProperties().AsMap()
	if props == nil {
		props = map[string]any{}
	}

	if _, err := w.Write.RegisterProfile(ctx, dbwrite.RegisterProfileParams{
		Properties: props,
		ID:         msg.GetProfileId(),
		ProjectID:  msg.GetProjectId(),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to register profile", slogx.Error(err),
			slog.String("profileId", msg.GetProfileId()))
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == pgerrcode.UniqueViolation {
			return natsworker.NewPermanentError(err).
				With("worker", "profile-register").
				With("profile_id", msg.GetProfileId()).
				With("project_id", msg.GetProjectId())
		}
		return err
	}

	return nil
}
