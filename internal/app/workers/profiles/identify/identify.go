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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
	"google.golang.org/protobuf/proto"

	profilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/profiles/v1"
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

	slog.InfoContext(ctx, "Starting profile identify worker...")
	return StartWorker(ctx, pgRO, pgW, natsClient)
}

func StartWorker(ctx context.Context, pgRO, pgW *pgxpool.Pool, natsClient *natsworker.NATSClient) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("profile-identify-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get profile identify consumer config: %w", err)
	}

	profileWorker := profiles.NewWorker(pgRO, pgW, nil)

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
	}

	worker, err := natsworker.NewWorker(config, messageProcessor)
	if err != nil {
		return err
	}

	return worker.Start(ctx, natsClient)
}

func handleIdentify(ctx context.Context, w *profiles.Worker, natsClient *natsworker.NATSClient, data []byte) error {
	msg := &profilesv1.ProfileIdentifyMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal identify message", slogx.Error(err))
		return &natsworker.PermanentError{Err: err}
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

	var mergedIntoProfileID string

	switch {
	case errors.Is(lookupErr, pgx.ErrNoRows):
		// No profile with this external_id — assign it to the anonymous profile.
		if _, err = qtx.SetProfileExternalID(ctx, dbwrite.SetProfileExternalIDParams{
			ExternalID: externalID,
			ID:         profileID,
			ProjectID:  projectID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				slog.WarnContext(ctx, "profile not found when setting external ID, may have been deleted",
					slog.String("profileId", profileID), slog.String("externalId", externalID))
				return nil
			}
			slog.ErrorContext(ctx, "failed setting external ID on profile", slogx.Error(err),
				slog.String("profileId", profileID), slog.String("externalId", externalID))
			return err
		}

	default:
		// Already identified — skip to avoid self-merge and deletion.
		if existing.ID == profileID {
			if err := tx.Commit(ctx); err != nil {
				slog.ErrorContext(ctx, "failed committing identify transaction", slogx.Error(err))
				return err
			}
			return nil
		}

		// Different profile owns this external_id — merge anonymous into existing.
		if _, err = qtx.MergeProfileProperties(ctx, dbwrite.MergeProfilePropertiesParams{
			SourceID:  profileID,
			TargetID:  existing.ID,
			ProjectID: projectID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Source profile no longer exists — likely merged on a previous attempt.
				// Continue with device reassignment to ensure no devices are orphaned.
				slog.WarnContext(ctx, "source profile missing during merge, continuing with device reassignment",
					slog.String("sourceId", profileID), slog.String("targetId", existing.ID))
			} else {
				slog.ErrorContext(ctx, "failed merging profile properties", slogx.Error(err),
					slog.String("sourceId", profileID), slog.String("targetId", existing.ID))
				return err
			}
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

		if err := qtx.DeleteProfileByIDAndProjectID(ctx, dbwrite.DeleteProfileByIDAndProjectIDParams{
			ID:        profileID,
			ProjectID: projectID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed deleting source profile", slogx.Error(err),
				slog.String("sourceId", profileID))
			return err
		}

		// Track which profile we merged into for alias recording
		mergedIntoProfileID = existing.ID
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing identify transaction", slogx.Error(err))
		return err
	}

	// Publish alias message for ClickHouse tracking only when a merge occurred
	if mergedIntoProfileID != "" {
		aliasMsg := &profilesv1.ProfileAliasMessage{
			AliasId:    profileID,
			ProfileId:  mergedIntoProfileID,
			ExternalId: externalID,
			ProjectId:  projectID,
		}

		aliasData, err := proto.Marshal(aliasMsg)
		if err != nil {
			slog.ErrorContext(ctx, "failed to marshal alias message after committed merge",
				slogx.Error(err),
				slog.String("aliasId", profileID),
				slog.String("profileId", mergedIntoProfileID))
		} else if err = natsClient.Publish(ctx, natsworker.ProfileAliasSubject, aliasData); err != nil {
			slog.ErrorContext(ctx, "failed to publish alias after committed merge, alias will be missing in ClickHouse",
				slogx.Error(err),
				slog.String("aliasId", profileID),
				slog.String("profileId", mergedIntoProfileID),
				slog.String("projectId", projectID))
		}
	}

	return nil
}
