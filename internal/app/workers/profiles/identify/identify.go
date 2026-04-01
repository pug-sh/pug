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
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
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
	profileID := msg.GetProfileId()
	externalID := msg.GetExternalId()

	existing, err := w.Read.GetProfileByProjectAndExternalID(ctx, dbread.GetProfileByProjectAndExternalIDParams{
		ProjectID:  projectID,
		ExternalID: externalID,
	})

	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed looking up profile by external ID", slogx.Error(err),
			slog.String("externalId", externalID))
		return err
	}

	lookupErr := err

	tx, err := w.PgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed starting transaction", slogx.Error(err))
		return err
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back transaction", slogx.Error(err))
		}
	}()

	qtx := w.Write.WithTx(tx)

	var (
		mergedIntoProfileID string
		// upsertProfile holds the target profile data to publish after commit.
		upsertID         string
		upsertExtID      string
		upsertProperties map[string]any
		// deletedProfileID is set in the merge case to soft-delete the source in CH.
		deletedProfileID string
	)

	switch {
	case errors.Is(lookupErr, pgx.ErrNoRows):
		// No profile with this external_id — assign it to the anonymous profile.
		updated, err := qtx.SetProfileExternalID(ctx, dbwrite.SetProfileExternalIDParams{
			ExternalID: externalID,
			ID:         profileID,
			ProjectID:  projectID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				slog.WarnContext(ctx, "profile not found when setting external ID, will retry",
					slog.String("profileId", profileID), slog.String("externalId", externalID))
				return fmt.Errorf("profile %s not found, register may not have completed yet", profileID)
			}
			slog.ErrorContext(ctx, "failed setting external ID on profile", slogx.Error(err),
				slog.String("profileId", profileID), slog.String("externalId", externalID))
			return err
		}
		upsertID = updated.ID
		upsertExtID = updated.ExternalID.String
		upsertProperties = updated.Properties

	default:
		// Already identified — skip merge and deletion but still sync current state to ClickHouse
		// (handles retries where PG succeeded but a prior NATS publish failed).
		if existing.ID == profileID {
			return publishUpsert(ctx, natsClient, existing.ID, projectID, existing.ExternalID.String, existing.Properties, false)
		}

		// Different profile owns this external_id — merge anonymous into existing.
		merged, err := qtx.MergeProfileProperties(ctx, dbwrite.MergeProfilePropertiesParams{
			SourceID:  profileID,
			TargetID:  existing.ID,
			ProjectID: projectID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// ErrNoRows means either the source or target profile no longer exists.
				// Most likely the source was already merged on a previous attempt.
				// Continue with device reassignment to ensure no devices are orphaned.
				slog.WarnContext(ctx, "profile missing during merge, continuing with device reassignment",
					slog.String("sourceId", profileID), slog.String("targetId", existing.ID))
				// Fall back to pre-merge snapshot of the target.
				upsertID = existing.ID
				upsertExtID = existing.ExternalID.String
				upsertProperties = existing.Properties
			} else {
				slog.ErrorContext(ctx, "failed merging profile properties", slogx.Error(err),
					slog.String("sourceId", profileID), slog.String("targetId", existing.ID))
				return err
			}
		} else {
			upsertID = merged.ID
			upsertExtID = merged.ExternalID.String
			upsertProperties = merged.Properties
		}

		if err := qtx.ReassignProfileDevices(ctx, dbwrite.ReassignProfileDevicesParams{
			TargetID:  existing.ID,
			SourceID:  profileID,
			ProjectID: projectID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed reassigning devices", slogx.Error(err),
				slog.String("sourceId", profileID), slog.String("targetId", existing.ID))
			return err
		}

		if _, err := qtx.DeleteProfileByIDAndProjectID(ctx, dbwrite.DeleteProfileByIDAndProjectIDParams{
			ID:        profileID,
			ProjectID: projectID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed deleting source profile", slogx.Error(err),
				slog.String("sourceId", profileID))
			return err
		}

		mergedIntoProfileID = existing.ID
		deletedProfileID = profileID
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing identify transaction", slogx.Error(err))
		return err
	}

	slog.InfoContext(ctx, "identify transaction committed",
		slog.String("profileId", upsertID),
		slog.String("deletedProfileId", deletedProfileID),
		slog.String("mergedIntoProfileId", mergedIntoProfileID))

	if upsertID == "" {
		return nil
	}

	// Publish upsert for the target profile.
	if err := publishUpsert(ctx, natsClient, upsertID, projectID, upsertExtID, upsertProperties, false); err != nil {
		return err
	}

	// Soft-delete the source profile in CH when a merge occurred.
	if deletedProfileID != "" {
		if err := publishUpsert(ctx, natsClient, deletedProfileID, projectID, "", nil, true); err != nil {
			return err
		}
	}

	// Publish alias message only when a merge occurred.
	if mergedIntoProfileID != "" {
		aliasMsg := &workerprofilesv1.ProfileAliasMessage{
			AliasId:    profileID,
			ProfileId:  mergedIntoProfileID,
			ExternalId: externalID,
			ProjectId:  projectID,
		}

		aliasData, err := proto.Marshal(aliasMsg)
		if err != nil {
			slog.ErrorContext(ctx, "failed marshalling alias message", slogx.Error(err),
				slog.String("aliasId", profileID), slog.String("profileId", mergedIntoProfileID))
			return fmt.Errorf("marshal alias message after committed merge: %w", err)
		}
		if err = natsClient.Publish(ctx, natsworker.ProfileAliasSubject, aliasData); err != nil {
			slog.ErrorContext(ctx, "failed publishing alias message", slogx.Error(err),
				slog.String("aliasId", profileID), slog.String("profileId", mergedIntoProfileID))
			return fmt.Errorf("publish alias after committed merge: %w", err)
		}
	}

	return nil
}

func publishUpsert(ctx context.Context, natsClient *natsworker.NATSClient, profileID, projectID, externalID string, properties map[string]any, isDeleted bool) error {
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
