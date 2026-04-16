package identify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"buf.build/go/protovalidate"
	"github.com/fivebitsio/cotton/internal/app/workers/profiles"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	sdkprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/profiles/v1"
	workerprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/workers/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/xid"
	"github.com/sethvargo/go-envconfig"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
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

	profileWorker := profiles.NewWorker(pgW)

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

	if err := protovalidate.Validate(msg); err != nil {
		slog.ErrorContext(ctx, "identify message failed validation", slogx.Error(err))
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
		ExternalID: postgres.NewText(externalID),
		Properties: traits,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to upsert profile", slogx.Error(err),
			slog.String("projectId", projectID),
			slog.String("externalId", externalID))
		return err
	}

	// Assign device to this profile. The SDK sends device_id on first identify
	// and on account switch (external_id changed) — not on every call.
	// Handles first-time linking (NULL → profile) and account switching (old → new).
	if deviceID := msg.GetDeviceId(); deviceID != "" {
		linked, err := w.Write.LinkDeviceToProfile(ctx, dbwrite.LinkDeviceToProfileParams{
			ProfileID: postgres.NewText(profile.ID),
			DeviceID:  deviceID,
			ProjectID: projectID,
		})
		if err != nil {
			slog.ErrorContext(ctx, "failed linking device to profile", slogx.Error(err),
				slog.String("projectId", projectID),
				slog.String("deviceId", deviceID),
				slog.String("profileId", profile.ID))
			return err
		}
		if linked == 0 {
			slog.WarnContext(ctx, "device not found for linking (may not exist yet)",
				slog.String("projectId", projectID),
				slog.String("deviceId", deviceID),
				slog.String("profileId", profile.ID))
		}
	}

	// No anonymous ID — just a trait update (and optional device link).
	if anonymousID == "" {
		return publishUpsert(ctx, natsClient, profile.ID, projectID, profile.ExternalID.String, profile.Properties, false, profile.CreateTime.Time, profile.UpdateTime.Time)
	}

	// Anonymous merge path: merge anonymous profile into the identified one.
	return mergeAnonymous(ctx, w, natsClient, projectID, anonymousID, profile)
}

func mergeAnonymous(ctx context.Context, w *profiles.Worker, natsClient *natsworker.NATSClient, projectID, anonymousID string, target dbwrite.Profile) error {
	// If the anonymous ID is the same as the target, skip the merge path.
	// Proceeding would attempt to merge a profile into itself:
	// MergeProfileProperties would be a no-op, but SoftDeleteProfileByIDAndProjectID
	// would soft-delete the very profile we just upserted. This happens when the SDK
	// re-identifies a profile that was already identified — the upsert returned
	// the existing row whose ID matches the anonymous_id sent by the client.
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
			slog.ErrorContext(ctx, "failed rolling back merge transaction", slogx.Error(err),
				slog.String("projectId", projectID),
				slog.String("anonymousId", anonymousID),
				slog.String("targetId", target.ID))
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
			// ErrNoRows means either the source (anonymous) or target is missing
			// from the join. Distinguish the two: if the target was concurrently
			// deleted (e.g. dashboard Delete), skip the merge entirely.
			var targetExists bool
			if err := tx.QueryRow(ctx,
				"select exists(select 1 from profiles where id = $1 and project_id = $2 and deletion_time is null)",
				target.ID, projectID).Scan(&targetExists); err != nil {
				slog.ErrorContext(ctx, "failed checking target profile existence", slogx.Error(err),
					slog.String("projectId", projectID),
					slog.String("targetId", target.ID))
				return err
			}
			if !targetExists {
				slog.WarnContext(ctx, "target profile deleted concurrently, skipping merge",
					slog.String("projectId", projectID),
					slog.String("anonymousId", anonymousID),
					slog.String("targetId", target.ID))
				// Release the connection before the NATS publish.
				if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
					slog.ErrorContext(ctx, "failed rolling back transaction after target-deleted skip", slogx.Error(err),
						slog.String("projectId", projectID),
						slog.String("anonymousId", anonymousID),
						slog.String("targetId", target.ID))
				}
				// The upsert in handleIdentify already succeeded in Postgres.
				// Publish it so ClickHouse stays consistent even though the
				// merge is skipped. The profile may be deleted again shortly,
				// but the upsert worker handles that idempotently.
				return publishUpsert(ctx, natsClient, target.ID, projectID, target.ExternalID.String, target.Properties, false, target.CreateTime.Time, target.UpdateTime.Time)
			}
			// Source (anonymous) is gone — expected on retry after a prior committed
			// merge. Proceed with the pre-upsert target snapshot; device reassignment
			// and delete are idempotent no-ops when the source is already gone.
			slog.WarnContext(ctx, "anonymous profile missing during merge (expected on retry)",
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
		TargetID:  postgres.NewText(target.ID),
		SourceID:  postgres.NewText(anonymousID),
		ProjectID: projectID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed reassigning devices", slogx.Error(err),
			slog.String("projectId", projectID),
			slog.String("anonymousId", anonymousID),
			slog.String("targetId", target.ID))
		return err
	}

	rowsDeleted, err := qtx.SoftDeleteProfileByIDAndProjectID(ctx, dbwrite.SoftDeleteProfileByIDAndProjectIDParams{
		ID:        anonymousID,
		ProjectID: projectID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed soft-deleting anonymous profile", slogx.Error(err),
			slog.String("projectId", projectID),
			slog.String("anonymousId", anonymousID))
		return err
	}
	if rowsDeleted == 0 {
		slog.DebugContext(ctx, "anonymous profile already soft-deleted (expected during retry)",
			slog.String("projectId", projectID),
			slog.String("anonymousId", anonymousID))
	}

	// Pre-commit: build and validate all outgoing NATS messages.
	// If any message is invalid, the deferred rollback aborts the transaction
	// so Postgres and ClickHouse stay consistent.
	targetUpsertData, err := buildUpsertData(ctx, merged.ID, projectID, merged.ExternalID.String, merged.Properties, false, merged.CreateTime.Time, merged.UpdateTime.Time)
	if err != nil {
		return err
	}

	// Soft-delete the anonymous profile in ClickHouse. We use time.Now() for
	// create_time and update_time because the anonymous profile was never fetched
	// and the soft-delete query returns only a row count. ReplacingMergeTree uses insert_time
	// (set at CH write time via DEFAULT now64(3)) for version ordering, so
	// these values don't affect dedup — they are purely informational.
	anonDeleteData, err := buildUpsertData(ctx, anonymousID, projectID, "", nil, true, time.Now(), time.Now())
	if err != nil {
		return err
	}

	aliasData, err := buildAliasData(ctx, anonymousID, target.ID, target.ExternalID.String, projectID)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing merge transaction", slogx.Error(err),
			slog.String("projectId", projectID),
			slog.String("anonymousId", anonymousID),
			slog.String("targetId", target.ID))
		return err
	}

	slog.InfoContext(ctx, "identify merge committed",
		slog.String("projectId", projectID),
		slog.String("targetId", target.ID),
		slog.String("anonymousId", anonymousID))

	// Post-commit: publish pre-validated messages.
	if err := natsClient.Publish(ctx, natsworker.ProfileUpsertSubject, targetUpsertData); err != nil {
		slog.ErrorContext(ctx, "failed publishing target upsert", slogx.Error(err),
			slog.String("targetId", merged.ID))
		return fmt.Errorf("post-commit publish target upsert: %w", err)
	}
	if err := natsClient.Publish(ctx, natsworker.ProfileUpsertSubject, anonDeleteData); err != nil {
		slog.ErrorContext(ctx, "failed publishing anonymous soft-delete", slogx.Error(err),
			slog.String("anonymousId", anonymousID))
		return fmt.Errorf("post-commit publish anonymous soft-delete: %w", err)
	}
	if err := natsClient.Publish(ctx, natsworker.ProfileAliasSubject, aliasData); err != nil {
		slog.ErrorContext(ctx, "failed publishing alias message", slogx.Error(err),
			slog.String("anonymousId", anonymousID), slog.String("targetId", target.ID))
		return fmt.Errorf("post-commit publish alias: %w", err)
	}

	return nil
}

// buildUpsertData constructs, validates, and marshals a ProfileUpsertMessage.
// Returns the serialized bytes ready for NATS publish, or a PermanentError
// if the message is invalid (e.g. missing required fields).
func buildUpsertData(ctx context.Context, profileID, projectID, externalID string, properties map[string]any, isDeleted bool, createTime, updateTime time.Time) ([]byte, error) {
	if properties == nil {
		properties = map[string]any{}
	}

	propsStruct, err := structpb.NewStruct(properties)
	if err != nil {
		slog.ErrorContext(ctx, "failed converting profile properties to struct", slogx.Error(err),
			slog.String("profileId", profileID))
		return nil, natsworker.NewPermanentError(err).
			With("worker", "profile-identify").
			With("profile_id", profileID)
	}

	upsertMsg := &workerprofilesv1.ProfileUpsertMessage{
		ProfileId:  proto.String(profileID),
		ProjectId:  proto.String(projectID),
		ExternalId: proto.String(externalID),
		Properties: propsStruct,
		IsDeleted:  proto.Bool(isDeleted),
		CreateTime: timestamppb.New(createTime),
		UpdateTime: timestamppb.New(updateTime),
	}

	if err := protovalidate.Validate(upsertMsg); err != nil {
		slog.ErrorContext(ctx, "constructed invalid upsert message", slogx.Error(err),
			slog.String("profileId", profileID))
		return nil, natsworker.NewPermanentError(err).
			With("worker", "profile-identify").
			With("profile_id", profileID)
	}

	data, err := proto.Marshal(upsertMsg)
	if err != nil {
		slog.ErrorContext(ctx, "failed marshalling profile upsert message", slogx.Error(err),
			slog.String("profileId", profileID))
		return nil, natsworker.NewPermanentError(err).
			With("worker", "profile-identify").
			With("profile_id", profileID)
	}

	return data, nil
}

// buildAliasData constructs, validates, and marshals a ProfileAliasMessage.
// Returns the serialized bytes ready for NATS publish, or a PermanentError
// if the message is invalid.
func buildAliasData(ctx context.Context, anonymousID, targetID, externalID, projectID string) ([]byte, error) {
	aliasMsg := &workerprofilesv1.ProfileAliasMessage{
		AliasId:    proto.String(anonymousID),
		ProfileId:  proto.String(targetID),
		ExternalId: proto.String(externalID),
		ProjectId:  proto.String(projectID),
	}

	if err := protovalidate.Validate(aliasMsg); err != nil {
		slog.ErrorContext(ctx, "constructed invalid alias message", slogx.Error(err),
			slog.String("anonymousId", anonymousID), slog.String("targetId", targetID))
		return nil, natsworker.NewPermanentError(err).
			With("worker", "profile-identify").
			With("anonymous_id", anonymousID)
	}

	data, err := proto.Marshal(aliasMsg)
	if err != nil {
		slog.ErrorContext(ctx, "failed marshalling alias message", slogx.Error(err),
			slog.String("anonymousId", anonymousID), slog.String("targetId", targetID))
		return nil, natsworker.NewPermanentError(err).
			With("worker", "profile-identify").
			With("anonymous_id", anonymousID)
	}

	return data, nil
}

// publishUpsert builds and publishes a profile state to the upsert NATS subject for
// ClickHouse sync. When isDeleted is true, the message acts as a soft-delete
// marker — the upsert worker writes the row with is_deleted=1, and queries
// must filter on is_deleted=0.
func publishUpsert(ctx context.Context, natsClient *natsworker.NATSClient, profileID, projectID, externalID string, properties map[string]any, isDeleted bool, createTime, updateTime time.Time) error {
	data, err := buildUpsertData(ctx, profileID, projectID, externalID, properties, isDeleted, createTime, updateTime)
	if err != nil {
		return err
	}

	if err := natsClient.Publish(ctx, natsworker.ProfileUpsertSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed publishing profile upsert", slogx.Error(err),
			slog.String("profileId", profileID))
		return fmt.Errorf("publish profile upsert: %w", err)
	}

	return nil
}
