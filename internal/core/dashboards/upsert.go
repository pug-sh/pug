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
// single transaction with a row lock on the dashboard (true last-write-wins
// under concurrent edits). Reconciliation has four branches:
//   - Empty id → insert (server assigns id).
//   - Populated id matching an existing tile → update (UPDATEs short-circuit
//     in SQL via payload_hash so byte-identical content doesn't bump
//     update_time).
//   - Existing tile id not in the request → delete.
//   - Populated id NOT matching any existing tile → rejected with
//     ErrDashboardTileNotFound (no insert-as-new fallback).
//
// Duplicate non-empty ids within one request are rejected with
// ErrDuplicateUpsertTileID. DELETE runs before INSERT/UPDATE so a swap-replace
// (drop tile A and insert a new tile reusing A's display_name) doesn't trip
// the partial unique index. Dashboard metadata is conditionally rewritten: it
// only bumps update_time when the metadata changed or any tile changed.
//
// Returns the post-state dashboard with tiles in **request order** so the
// client can map server-assigned ids back to its draft. The reload happens
// after commit (not inside the tx) — a concurrent DeleteDashboard between
// commit and reload is observable as ErrDashboardNotFound and is logged
// distinctly so operators can tell it apart from a never-existed 404.
func (s *Service) UpsertDashboard(ctx context.Context, projectID, dashboardID string, in UpsertDashboardInput) (result DashboardWithTiles, retErr error) {
	// Reject duplicate non-empty tile ids in one request. Without this the loop
	// would run UPDATE twice for the same row (last-write-wins silently) and
	// the response would include the same tile twice. protovalidate can't
	// express batch-level cross-element checks on a repeated field, hence the
	// Go-side check.
	seenIDs := make(map[string]struct{}, len(in.Tiles))
	for i, t := range in.Tiles {
		if t.ID == "" {
			continue
		}
		if _, dup := seenIDs[t.ID]; dup {
			// Client-input validation error — log at WARN and return the sentinel
			// without recordServiceError, so a buggy client doesn't manufacture
			// error-rate noise. Mirrors the "tile id not on dashboard" branch.
			slog.WarnContext(ctx, "upsert rejected: duplicate tile id in request",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
				slog.String("tile_id", t.ID),
				slog.Int("tile_index", i),
			)
			return DashboardWithTiles{}, ErrDuplicateUpsertTileID
		}
		seenIDs[t.ID] = struct{}{}
	}

	// Pre-encode all tiles outside the transaction so we fail fast before
	// opening a transaction we'd then have to roll back.
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
			// Capture the function-level error so the rollback warning is
			// triageable on its own — without this, an operator seeing the
			// rollback log has to correlate by trace id to find the original
			// failure.
			attrs := []any{slogx.Error(rbErr),
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID)}
			if retErr != nil {
				attrs = append(attrs, slog.String("triggering_error", retErr.Error()))
			}
			slog.WarnContext(ctx, "failed to rollback upsert transaction", attrs...)
		}
	}()

	writeTx := s.write.WithTx(tx)
	readTx := s.read.WithTx(tx)

	// Acquire a row lock on the dashboard for the rest of this tx. Without it,
	// two concurrent Upserts under READ COMMITTED can both insert their own
	// tile (each sees an empty existing set) and the test's last-write-wins
	// contract becomes timing-dependent. The lock also verifies existence so
	// every subsequent statement's WHERE clause has something to match.
	if _, err := readTx.LockDashboardByIDAndProjectID(ctx, dbread.LockDashboardByIDAndProjectIDParams{
		ID:        dashboardID,
		ProjectID: projectID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.DebugContext(ctx, "upsert dashboard: not found",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
			)
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

	// finalIDs[i] is the post-write id of the i-th request tile. Pre-assign new
	// ids for empty-id tiles (inserts) and validate that populated ids exist
	// before we touch any rows, so a swap-replace (drop tile A and insert a new
	// tile reusing A's display_name) doesn't trip the partial unique index —
	// DELETE runs before INSERT/UPDATE below.
	finalIDs := make([]string, len(in.Tiles))
	for i, tile := range in.Tiles {
		if tile.ID == "" {
			finalIDs[i] = xid.New().String()
			continue
		}
		if _, ok := existingSet[tile.ID]; !ok {
			slog.WarnContext(ctx, "upsert rejected: tile id not on dashboard",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
				slog.String("tile_id", tile.ID),
				slog.Int("tile_index", i),
			)
			return DashboardWithTiles{}, ErrDashboardTileNotFound
		}
		finalIDs[i] = tile.ID
	}

	// DELETE first so a new tile in this request can reuse a dropped tile's
	// display_name within a single Upsert call.
	deleted, err := writeTx.DeleteDashboardTilesNotIn(ctx, dbwrite.DeleteDashboardTilesNotInParams{
		DashboardID: dashboardID,
		ProjectID:   projectID,
		KeepIds:     finalIDs,
	})
	if err != nil {
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to delete tiles during upsert", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}
	tilesChanged := deleted > 0

	for i, tile := range in.Tiles {
		enc := encoded[i]
		switch tile.ID {
		case "":
			if _, err := writeTx.CreateDashboardTile(ctx, dbwrite.CreateDashboardTileParams{
				ID:            finalIDs[i],
				DashboardID:   dashboardID,
				ProjectID:     projectID,
				Kind:          int16(enc.Kind),
				ViewMode:      enc.ViewMode,
				DisplayName:   tile.Payload.DisplayName,
				Description:   tile.Payload.Description,
				InsightQuery:  enc.InsightQuery,
				MarkdownBody:  enc.MarkdownBody,
				Position:      enc.Position,
				Compare:       enc.Compare,
				Thresholds:    enc.Thresholds,
				Header:        enc.Header,
				Visualization: enc.Visualization,
				PayloadHash:   enc.PayloadHash,
			}); err != nil {
				// CreateDashboardTile is `INSERT ... SELECT ... FROM dashboards d
				// WHERE d.id = $X AND d.project_id = $Y` — zero rows when the
				// dashboard row was deleted concurrent with this tx. Surface as
				// 404 rather than CodeInternal. The row lock acquired upfront
				// makes this very unlikely but not impossible (lock acquired
				// AFTER a sibling tx's DELETE commits).
				if errors.Is(err, pgx.ErrNoRows) {
					return DashboardWithTiles{}, ErrDashboardNotFound
				}
				if conflict := translateUniqueViolation(err); conflict != nil {
					return DashboardWithTiles{}, conflict
				}
				return DashboardWithTiles{}, recordServiceError(ctx, "failed to insert tile during upsert", err,
					slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID), slog.Int("tile_index", i))
			}
			tilesChanged = true

		default:
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
				Position:      enc.Position,
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
		}
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
		// concurrent DeleteDashboard could race. The write is already committed,
		// so the client will see 404 for a write that did succeed — log a
		// distinct WARN so operators can tell this apart from a genuine "never
		// existed" 404 in incident triage.
		if errors.Is(err, ErrDashboardNotFound) {
			slog.WarnContext(ctx, "upsert committed but post-reload missed dashboard (concurrent delete)",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
			)
		}
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
