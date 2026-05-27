package dashboards

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// UpsertDashboardInput is the service-layer projection of
// DashboardsServiceUpsertRequest. The handler builds this from the proto.
type UpsertDashboardInput struct {
	DisplayName        string
	Description        string
	DefaultTimeRange   commonv1.TimeRangePreset
	DefaultGranularity insightsv1.Granularity
	Tiles              []UpsertTileInput
}

// UpsertTileInput is one tile within an Upsert call. An empty ID means
// "insert a new tile (server assigns id)"; a populated ID means "update an
// existing tile with this content". An ID that doesn't match an existing tile
// is rejected with ErrDashboardTileNotFound.
type UpsertTileInput struct {
	ID      string
	Payload TilePayload
}

// UpsertDashboard reconciles a dashboard's metadata and the full tile set in a
// single transaction. Tiles with empty id are inserted (server assigns ids);
// tiles with matching ids are updated (UPDATEs short-circuit in SQL via
// payload_hash so byte-identical content doesn't bump update_time); existing
// tiles whose id is not in the request are deleted. Dashboard metadata is
// conditionally rewritten: it only bumps update_time when the metadata changed
// or any tile changed.
//
// Returns the post-state dashboard with tiles in **request order** so the
// client can map server-assigned ids back to its draft. The reload happens
// after commit (not inside the tx) — under last-write-wins semantics a
// concurrent edit between commit and reload is observable in the result, but
// the spec accepts that.
func (s *Service) UpsertDashboard(ctx context.Context, projectID, dashboardID string, in UpsertDashboardInput) (DashboardWithTiles, error) {
	// Pre-encode all tiles outside the transaction so payload errors don't roll
	// back DB work that could otherwise have succeeded.
	encoded := make([]EncodedTilePayload, len(in.Tiles))
	for i, t := range in.Tiles {
		enc, err := t.Payload.Encode()
		if err != nil {
			slog.ErrorContext(ctx, "failed to encode tile payload for upsert",
				slogx.Error(err),
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
				slog.Int("tile_index", i),
			)
			telemetry.RecordError(ctx, err)
			return DashboardWithTiles{}, err
		}
		encoded[i] = enc
	}

	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to begin upsert transaction", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			slog.WarnContext(ctx, "failed to rollback upsert transaction",
				slogx.Error(rbErr),
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID))
		}
	}()

	writeTx := s.write.WithTx(tx)
	readTx := s.read.WithTx(tx)

	// Verify the dashboard exists in this project; without this, every
	// subsequent statement's WHERE clause silently no-ops.
	if _, err := readTx.GetDashboardByIDAndProjectID(ctx, dbread.GetDashboardByIDAndProjectIDParams{
		ID:        dashboardID,
		ProjectID: projectID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DashboardWithTiles{}, ErrDashboardNotFound
		}
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to load dashboard for upsert", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}

	existingIDs, err := readTx.ListDashboardTileIDsByDashboardIDAndProjectID(ctx, dbread.ListDashboardTileIDsByDashboardIDAndProjectIDParams{
		DashboardID: dashboardID,
		ProjectID:   projectID,
	})
	if err != nil {
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to list existing tile ids for upsert", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}
	existingSet := make(map[string]struct{}, len(existingIDs))
	for _, id := range existingIDs {
		existingSet[id] = struct{}{}
	}

	// finalIDs[i] is the post-write id of the i-th request tile; populated for
	// inserts (with the freshly assigned id) and updates (the existing id).
	finalIDs := make([]string, len(in.Tiles))
	tilesChanged := false
	for i, tile := range in.Tiles {
		enc := encoded[i]
		switch {
		case tile.ID == "":
			newID := xid.New().String()
			if _, err := writeTx.CreateDashboardTile(ctx, dbwrite.CreateDashboardTileParams{
				ID:            newID,
				DashboardID:   dashboardID,
				ProjectID:     projectID,
				Kind:          int16(enc.Kind),
				ViewMode:      enc.ViewMode,
				DisplayName:   tile.Payload.DisplayName,
				Description:   tile.Payload.Description,
				InsightQuery:  enc.InsightQuery,
				MarkdownBody:  enc.MarkdownBody,
				Layouts:       enc.Layouts,
				Compare:       enc.Compare,
				Thresholds:    enc.Thresholds,
				Header:        enc.Header,
				Visualization: enc.Visualization,
				PayloadHash:   enc.PayloadHash,
			}); err != nil {
				if conflict := translateUniqueViolation(err); conflict != nil {
					return DashboardWithTiles{}, conflict
				}
				return DashboardWithTiles{}, recordServiceError(ctx, "failed to insert tile during upsert", err,
					slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID), slog.Int("tile_index", i))
			}
			finalIDs[i] = newID
			tilesChanged = true

		default:
			if _, ok := existingSet[tile.ID]; !ok {
				return DashboardWithTiles{}, ErrDashboardTileNotFound
			}
			rows, err := writeTx.UpsertDashboardTileUpdate(ctx, dbwrite.UpsertDashboardTileUpdateParams{
				ID:            tile.ID,
				DashboardID:   dashboardID,
				ProjectID:     projectID,
				Kind:          int16(enc.Kind),
				ViewMode:      enc.ViewMode,
				DisplayName:   tile.Payload.DisplayName,
				Description:   tile.Payload.Description,
				InsightQuery:  enc.InsightQuery,
				MarkdownBody:  enc.MarkdownBody,
				Layouts:       enc.Layouts,
				Compare:       enc.Compare,
				Thresholds:    enc.Thresholds,
				Header:        enc.Header,
				Visualization: enc.Visualization,
				PayloadHash:   enc.PayloadHash,
			})
			if err != nil {
				if conflict := translateUniqueViolation(err); conflict != nil {
					return DashboardWithTiles{}, conflict
				}
				return DashboardWithTiles{}, recordServiceError(ctx, "failed to update tile during upsert", err,
					slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID), slog.String("tile_id", tile.ID))
			}
			if rows > 0 {
				tilesChanged = true
			}
			finalIDs[i] = tile.ID
		}
	}

	// finalIDs contains every tile that should remain on the dashboard
	// (inserts + updates). DeleteDashboardTilesNotIn removes everything else.
	keepIDs := append([]string{}, finalIDs...)
	deleted, err := writeTx.DeleteDashboardTilesNotIn(ctx, dbwrite.DeleteDashboardTilesNotInParams{
		DashboardID: dashboardID,
		ProjectID:   projectID,
		KeepIds:     keepIDs,
	})
	if err != nil {
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to delete tiles during upsert", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}
	if deleted > 0 {
		tilesChanged = true
	}

	// Conditional dashboard-metadata write. The trigger only fires when this
	// UPDATE matches a row; the SQL WHERE makes the UPDATE a no-op when neither
	// metadata changed nor any tile changed.
	if _, err := writeTx.UpsertDashboardMetadata(ctx, dbwrite.UpsertDashboardMetadataParams{
		ID:                 dashboardID,
		ProjectID:          projectID,
		DisplayName:        in.DisplayName,
		Description:        in.Description,
		DefaultTimeRange:   dashboardDefaultTimeRangeDBName(in.DefaultTimeRange),
		DefaultGranularity: dashboardGranularityDBName(in.DefaultGranularity),
		TilesChanged:       tilesChanged,
	}); err != nil {
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to update dashboard metadata during upsert", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}

	if err := tx.Commit(ctx); err != nil {
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to commit upsert transaction", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}

	fresh, err := s.GetDashboard(ctx, projectID, dashboardID)
	if err != nil {
		// GetDashboard already records / classifies the error. ErrDashboardNotFound
		// shouldn't happen here (we just upserted into the same dashboard) but a
		// concurrent DeleteDashboard could race; propagate verbatim.
		return DashboardWithTiles{}, err
	}

	// Reorder tiles to match request order so the client can map newly-assigned
	// ids back to its draft.
	tilesByID := make(map[string]dbread.DashboardTile, len(fresh.Tiles))
	for _, t := range fresh.Tiles {
		tilesByID[t.ID] = t
	}
	ordered := make([]dbread.DashboardTile, 0, len(finalIDs))
	for _, id := range finalIDs {
		if t, ok := tilesByID[id]; ok {
			ordered = append(ordered, t)
		}
	}
	return DashboardWithTiles{
		Dashboard: fresh.Dashboard,
		Tiles:     ordered,
	}, nil
}
