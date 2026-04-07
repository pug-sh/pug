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
	return StartWorker(ctx, pgW, natsClient)
}

func StartWorker(ctx context.Context, pgW *pgxpool.Pool, natsClient *natsworker.NATSClient) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("profile-identify-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get profile identify consumer config: %w", err)
	}

	profileWorker := profiles.NewWorker(nil, pgW)

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

	// Upsert the identified profile — creates if new, merges traits if exists
	// (new traits overwrite existing keys via jsonb_shallow_merge).
	profile, err := w.Write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
		ID:         xid.New().String(),
		ProjectID:  projectID,
		ExternalID: pgtype.Text{String: externalID, Valid: true},
		Properties: traits,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to upsert profile", slogx.Error(err),
			slog.String("projectId", projectID),
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
	// This happens when the SDK re-identifies a profile that was already
	// identified — the upsert returned the existing row whose ID matches
	// the anonymous_id sent by the client.
	if anonymousID == target.ID {
		return publishUpsert(ctx, natsClient, target.ID, projectID, target.ExternalID.String, target.Properties, false, target.CreateTime.Time, target.UpdateTime.Time)
	}

	tx, err := w.PgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed starting merge transaction", slogx.Error(err),
			slog.String("projectId", projectID),
			slog.String("anonymousId", anonymousID),
			slog.String("targetId", target.ID))
		return err
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back merge transaction", slogx.Error(err))
		}
	}()

	qtx := w.Write.WithTx(tx)

	// Merge properties from anonymous into target (target keys take precedence
	// via jsonb_shallow_merge).
	merged, err := qtx.MergeProfileProperties(ctx, dbwrite.MergeProfilePropertiesParams{
		SourceID:  anonymousID,
		TargetID:  target.ID,
		ProjectID: projectID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ErrNoRows: either the source (anonymous) or target is missing from the
			// join. The expected case is a retry where the anonymous profile was
			// already merged and deleted on a prior attempt. The target was just
			// upserted by UpsertProfileByExternalID moments before, so concurrent
			// target deletion (e.g. dashboard Delete) is the only other possibility
			// — an extremely narrow race window. Proceed with the pre-merge target
			// snapshot; device reassignment and delete are idempotent no-ops when
			// the source is already gone.
			slog.WarnContext(ctx, "profile missing during merge, using pre-merge snapshot",
				slog.String("projectId", projectID),
				slog.String("anonymousId", anonymousID),
				slog.String("targetId", target.ID))
			merged = target
		} else {
			slog.ErrorContext(ctx, "failed merging profile properties", slogx.Error(err),
				slog.String("projectId", projectID),
				slog.String("anonymousId", anonymousID),
				slog.String("targetId", target.ID))
			return err
		}
	}

	if err := qtx.ReassignProfileDevices(ctx, dbwrite.ReassignProfileDevicesParams{
		TargetID:  target.ID,
		SourceID:  anonymousID,
		ProjectID: projectID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed reassigning devices", slogx.Error(err),
			slog.String("projectId", projectID),
			slog.String("anonymousId", anonymousID),
			slog.String("targetId", target.ID))
		return err
	}

	rowsDeleted, err := qtx.DeleteProfileByIDAndProjectID(ctx, dbwrite.DeleteProfileByIDAndProjectIDParams{
		ID:        anonymousID,
		ProjectID: projectID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed deleting anonymous profile", slogx.Error(err),
			slog.String("projectId", projectID),
			slog.String("anonymousId", anonymousID))
		return err
	}
	if rowsDeleted == 0 {
		slog.DebugContext(ctx, "anonymous profile already deleted (expected during retry)",
			slog.String("projectId", projectID),
			slog.String("anonymousId", anonymousID))
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing merge transaction", slogx.Error(err),
			slog.String("projectId", projectID),
			slog.String("anonymousId", anonymousID),
			slog.String("targetId", target.ID))
		return err
	}

	slog.InfoContext(ctx, "identify merge committed",
		slog.String("targetId", target.ID),
		slog.String("anonymousId", anonymousID))

	// Publish upsert for target.
	if err := publishUpsert(ctx, natsClient, merged.ID, projectID, merged.ExternalID.String, merged.Properties, false, merged.CreateTime.Time, merged.UpdateTime.Time); err != nil {
		return fmt.Errorf("post-commit publish target upsert: %w", err)
	}

	// Soft-delete the anonymous profile in ClickHouse.
	if err := publishUpsert(ctx, natsClient, anonymousID, projectID, "", nil, true, time.Now(), time.Now()); err != nil {
		return fmt.Errorf("post-commit publish anonymous soft-delete: %w", err)
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
		return fmt.Errorf("post-commit publish alias: %w", err)
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
