package profiles

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	profilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
)

func (w *Worker) handleIdentify(ctx context.Context, msg *profilesv1.ProfileOperationMessage) error {
	projectID := msg.GetProjectId()
	profileID := msg.GetProfileId()
	externalID := msg.GetExternalId()

	tx, err := w.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed starting transaction", slogx.Error(err))
		return err
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back transaction", slogx.Error(err))
		}
	}()

	qtx := w.write.WithTx(tx)

	existing, err := qtx.GetProfileByProjectAndExternalID(ctx, dbwrite.GetProfileByProjectAndExternalIDParams{
		ProjectID:  projectID,
		ExternalID: externalID,
	})

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No profile with this external_id — assign it to the anonymous profile.
		if _, err = qtx.SetProfileExternalID(ctx, dbwrite.SetProfileExternalIDParams{
			ExternalID: externalID,
			ID:         profileID,
			ProjectID:  projectID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed setting external ID on profile", slogx.Error(err),
				slog.String("profileId", profileID), slog.String("externalId", externalID))
			return err
		}

	case err != nil:
		slog.ErrorContext(ctx, "failed looking up profile by external ID", slogx.Error(err),
			slog.String("externalId", externalID))
		return err

	case existing.ID == profileID:
		// Already identified — no-op.
		return nil

	default:
		// Different profile owns this external_id — merge anonymous into existing.
		if _, err = qtx.MergeProfileProperties(ctx, dbwrite.MergeProfilePropertiesParams{
			SourceID:  profileID,
			TargetID:  existing.ID,
			ProjectID: projectID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed merging profile properties", slogx.Error(err),
				slog.String("sourceId", profileID), slog.String("targetId", existing.ID))
			return err
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

		if err := w.ch.Exec(ctx,
			"INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)",
			profileID, existing.ID, externalID, projectID,
		); err != nil {
			slog.ErrorContext(ctx, "failed inserting profile alias into ClickHouse", slogx.Error(err),
				slog.String("aliasId", profileID), slog.String("profileId", existing.ID))
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing identify transaction", slogx.Error(err))
		return err
	}

	return nil
}
