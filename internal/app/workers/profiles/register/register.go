package register

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fivebitsio/cotton/internal/app/workers/profiles"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	workerprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/workers/profiles/v1"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	sdkprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
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
		return handleRegister(ctx, profileWorker, natsClient, msg.Data())
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

func handleRegister(ctx context.Context, w *profiles.Worker, natsClient *natsworker.NATSClient, data []byte) error {
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

	profile, err := w.Write.RegisterProfile(ctx, dbwrite.RegisterProfileParams{
		Properties: props,
		ID:         msg.GetProfileId(),
		ProjectID:  msg.GetProjectId(),
	})
	if err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == pgerrcode.UniqueViolation {
			// Profile already exists — likely a retry after PG succeeded but NATS publish failed.
			// Re-read so we can still sync to ClickHouse.
			slog.InfoContext(ctx, "profile already registered, re-syncing to ClickHouse",
				slog.String("profileId", msg.GetProfileId()))
			existing, readErr := w.Read.GetProfileByIDAndProjectID(ctx, dbread.GetProfileByIDAndProjectIDParams{
				ID:        msg.GetProfileId(),
				ProjectID: msg.GetProjectId(),
			})
			if readErr != nil {
				slog.ErrorContext(ctx, "failed reading existing profile after unique violation", slogx.Error(readErr),
					slog.String("profileId", msg.GetProfileId()))
				return natsworker.NewPermanentError(readErr).
					With("worker", "profile-register").
					With("profile_id", msg.GetProfileId())
			}
			return publishRegisterUpsert(ctx, natsClient, existing.ID, existing.ProjectID, existing.ExternalID.String, existing.Properties, existing.CreateTime.Time, existing.UpdateTime.Time)
		}
		slog.ErrorContext(ctx, "failed to register profile", slogx.Error(err),
			slog.String("profileId", msg.GetProfileId()))
		return err
	}

	return publishRegisterUpsert(ctx, natsClient, profile.ID, profile.ProjectID, profile.ExternalID.String, profile.Properties, profile.CreateTime.Time, profile.UpdateTime.Time)
}

func publishRegisterUpsert(ctx context.Context, natsClient *natsworker.NATSClient, profileID, projectID, externalID string, properties map[string]any, createTime, updateTime time.Time) error {
	propsStruct, err := structpb.NewStruct(properties)
	if err != nil {
		slog.ErrorContext(ctx, "failed converting profile properties to struct", slogx.Error(err),
			slog.String("profileId", profileID))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-register").
			With("profile_id", profileID)
	}

	upsertMsg := &workerprofilesv1.ProfileUpsertMessage{
		ProfileId:  profileID,
		ProjectId:  projectID,
		ExternalId: externalID,
		Properties: propsStruct,
		CreateTime: timestamppb.New(createTime),
		UpdateTime: timestamppb.New(updateTime),
	}

	upsertData, err := proto.Marshal(upsertMsg)
	if err != nil {
		slog.ErrorContext(ctx, "failed marshalling profile upsert message", slogx.Error(err),
			slog.String("profileId", profileID))
		return fmt.Errorf("marshal profile upsert message: %w", err)
	}

	if err := natsClient.Publish(ctx, natsworker.ProfileUpsertSubject, upsertData); err != nil {
		slog.ErrorContext(ctx, "failed publishing profile upsert", slogx.Error(err),
			slog.String("profileId", profileID))
		return fmt.Errorf("publish profile upsert: %w", err)
	}

	return nil
}
