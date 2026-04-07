package identify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fivebitsio/cotton/internal/app/workers/profiles"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	sdkprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1"
	workerprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/workers/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/xid"
	"github.com/sethvargo/go-envconfig"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
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

	slog.InfoContext(ctx, "Starting profile identify worker...")
	return StartWorker(ctx, pgRO, pgW, natsClient)
}

func StartWorker(ctx context.Context, pgRO, pgW *pgxpool.Pool, natsClient *natsworker.NATSClient) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("profile-identify-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get profile identify consumer config: %w", err)
	}

	profileWorker := profiles.NewWorker(pgRO, pgW)

	messageProcessor := func(ctx context.Context, msg jetstream.Msg) error {
		return handleIdentify(ctx, profileWorker, natsClient, msg.Data())
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
		DLQSubject:        natsworker.DLQProfilesIdentifySubject,
	}

	worker, err := natsworker.NewWorker(config, messageProcessor, natsClient)
	if err != nil {
		return err
	}

	return worker.Start(ctx)
}

func handleIdentify(ctx context.Context, w *profiles.Worker, natsClient *natsworker.NATSClient, data []byte) error {
	msg := &sdkprofilesv1.ProfileIdentifyMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal identify message", slogx.Error(err))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-identify")
	}

	projectID := msg.GetProjectId()
	externalID := msg.GetExternalId()
	anonymousID := msg.GetAnonymousId()

	traits := msg.GetTraits().AsMap()
	if traits == nil {
		traits = map[string]any{}
	}

	// Upsert the identified profile — creates if new, merges traits if exists.
	profile, err := w.Write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         xid.New().String(),
		ProjectID:  projectID,
		ExternalID: pgtype.Text{String: externalID, Valid: true},
		Properties: traits,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to upsert profile", slogx.Error(err),
			slog.String("externalId", externalID))
		return err
	}

	// No anonymous profile to merge — publish upsert and we're done.
	if anonymousID == "" {
		return publishUpsert(ctx, natsClient, profile.ID, projectID, profile.ExternalID.String, profile.Properties, false, profile.CreateTime.Time, profile.UpdateTime.Time)
	}

	// Anonymous merge path: merge anonymous profile into the identified one.
	return mergeAnonymous(ctx, w, natsClient, projectID, externalID, anonymousID, profile)
}

func mergeAnonymous(ctx context.Context, w *profiles.Worker, natsClient *natsworker.NATSClient, projectID, externalID, anonymousID string, target dbwrite.Profile) error {
	// If the anonymous ID is the same as the target, nothing to merge.
	if anonymousID == target.ID {
		return publishUpsert(ctx, natsClient, target.ID, projectID, target.ExternalID.String, target.Properties, false, target.CreateTime.Time, target.UpdateTime.Time)
	}

	tx, err := w.PgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed starting merge transaction", slogx.Error(err))
		return err
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back merge transaction", slogx.Error(err))
		}
	}()

	qtx := w.Write.WithTx(tx)

	// Merge properties from anonymous into target.
	merged, err := qtx.MergeProfileProperties(ctx, dbwrite.MergeProfilePropertiesParams{
		SourceID:  anonymousID,
		TargetID:  target.ID,
		ProjectID: projectID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Anonymous profile already deleted (retry). Use pre-merge target snapshot.
			slog.WarnContext(ctx, "anonymous profile missing during merge, using pre-merge snapshot",
				slog.String("anonymousId", anonymousID), slog.String("targetId", target.ID))
			merged = target
		} else {
			slog.ErrorContext(ctx, "failed merging profile properties", slogx.Error(err),
				slog.String("anonymousId", anonymousID), slog.String("targetId", target.ID))
			return err
		}
	}

	if err := qtx.ReassignProfileDevices(ctx, dbwrite.ReassignProfileDevicesParams{
		TargetID:  target.ID,
		SourceID:  anonymousID,
		ProjectID: projectID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed reassigning devices", slogx.Error(err),
			slog.String("anonymousId", anonymousID), slog.String("targetId", target.ID))
		return err
	}

	if _, err := qtx.DeleteProfileByIDAndProjectID(ctx, dbwrite.DeleteProfileByIDAndProjectIDParams{
		ID:        anonymousID,
		ProjectID: projectID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed deleting anonymous profile", slogx.Error(err),
			slog.String("anonymousId", anonymousID))
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing merge transaction", slogx.Error(err))
		return err
	}

	slog.InfoContext(ctx, "identify merge committed",
		slog.String("targetId", target.ID),
		slog.String("anonymousId", anonymousID))

	// Publish upsert for target.
	if err := publishUpsert(ctx, natsClient, merged.ID, projectID, merged.ExternalID.String, merged.Properties, false, merged.CreateTime.Time, merged.UpdateTime.Time); err != nil {
		return err
	}

	// Soft-delete the anonymous profile in ClickHouse.
	if err := publishUpsert(ctx, natsClient, anonymousID, projectID, "", nil, true, time.Now(), time.Now()); err != nil {
		return err
	}

	// Publish alias for traceability.
	aliasMsg := &workerprofilesv1.ProfileAliasMessage{
		AliasId:    anonymousID,
		ProfileId:  target.ID,
		ExternalId: externalID,
		ProjectId:  projectID,
	}

	aliasData, err := proto.Marshal(aliasMsg)
	if err != nil {
		slog.ErrorContext(ctx, "failed marshalling alias message", slogx.Error(err),
			slog.String("anonymousId", anonymousID), slog.String("targetId", target.ID))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-identify").
			With("anonymous_id", anonymousID)
	}
	if err = natsClient.Publish(ctx, natsworker.ProfileAliasSubject, aliasData); err != nil {
		slog.ErrorContext(ctx, "failed publishing alias message", slogx.Error(err),
			slog.String("anonymousId", anonymousID), slog.String("targetId", target.ID))
		return fmt.Errorf("publish alias after committed merge: %w", err)
	}

	return nil
}

func publishUpsert(ctx context.Context, natsClient *natsworker.NATSClient, profileID, projectID, externalID string, properties map[string]any, isDeleted bool, createTime, updateTime time.Time) error {
	if properties == nil {
		properties = map[string]any{}
	}

	propsStruct, err := structpb.NewStruct(properties)
	if err != nil {
		slog.ErrorContext(ctx, "failed converting profile properties to struct", slogx.Error(err),
			slog.String("profileId", profileID))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-identify").
			With("profile_id", profileID)
	}

	upsertMsg := &workerprofilesv1.ProfileUpsertMessage{
		ProfileId:  profileID,
		ProjectId:  projectID,
		ExternalId: externalID,
		Properties: propsStruct,
		IsDeleted:  isDeleted,
		CreateTime: timestamppb.New(createTime),
		UpdateTime: timestamppb.New(updateTime),
	}

	upsertData, err := proto.Marshal(upsertMsg)
	if err != nil {
		slog.ErrorContext(ctx, "failed marshalling profile upsert message", slogx.Error(err),
			slog.String("profileId", profileID))
		return natsworker.NewPermanentError(err).
			With("worker", "profile-identify").
			With("profile_id", profileID)
	}

	if err := natsClient.Publish(ctx, natsworker.ProfileUpsertSubject, upsertData); err != nil {
		slog.ErrorContext(ctx, "failed publishing profile upsert", slogx.Error(err),
			slog.String("profileId", profileID))
		return fmt.Errorf("publish profile upsert: %w", err)
	}

	return nil
}
