package dashboards

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

func dbwriteShareToDbread(share dbwrite.DashboardShare) dbread.DashboardShare {
	return dbread.DashboardShare{
		ID:          share.ID,
		DashboardID: share.DashboardID,
		ProjectID:   share.ProjectID,
		Enabled:     share.Enabled,
		CreateTime:  share.CreateTime,
		UpdateTime:  share.UpdateTime,
	}
}

func (s *Service) lookupShare(ctx context.Context, dashboardID string) (*dbread.DashboardShare, error) {
	share, err := s.read.GetDashboardShareByDashboardID(ctx, dashboardID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, recordServiceError(ctx, "failed to get dashboard share", err,
			slog.String("dashboard_id", dashboardID))
	}
	if !share.Enabled {
		return nil, nil
	}
	return &share, nil
}

// setShare upserts the share row for a dashboard, toggling enabled without
// rotating the share id on re-enable.
func (s *Service) setShare(ctx context.Context, projectID, dashboardID string, enabled bool) (dbwrite.DashboardShare, error) {
	share, err := s.write.UpsertDashboardShare(ctx, dbwrite.UpsertDashboardShareParams{
		ID:          xid.New().String(),
		DashboardID: dashboardID,
		ProjectID:   projectID,
		Enabled:     enabled,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.DebugContext(ctx, "set dashboard share: dashboard not found",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
			)
			return dbwrite.DashboardShare{}, ErrDashboardNotFound
		}
		return dbwrite.DashboardShare{}, recordServiceError(ctx, "failed to set dashboard share", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}
	return share, nil
}

// GetSharedDashboard loads a dashboard and its tiles via an enabled share id.
func (s *Service) GetSharedDashboard(ctx context.Context, shareID string) (DashboardWithTiles, error) {
	share, err := s.read.GetEnabledDashboardShareByID(ctx, shareID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.DebugContext(ctx, "get shared dashboard: share not found or disabled",
				slog.String("share_id", shareID),
			)
			return DashboardWithTiles{}, ErrDashboardNotFound
		}
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to get dashboard share", err,
			slog.String("share_id", shareID))
	}

	dashboard, err := s.read.GetDashboardByIDAndProjectID(ctx, dbread.GetDashboardByIDAndProjectIDParams{
		ID:        share.DashboardID,
		ProjectID: share.ProjectID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "get shared dashboard: share references missing dashboard",
				slog.String("share_id", shareID),
				slog.String("dashboard_id", share.DashboardID),
			)
			return DashboardWithTiles{}, ErrDashboardNotFound
		}
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to get shared dashboard", err,
			slog.String("share_id", shareID), slog.String("dashboard_id", share.DashboardID))
	}

	tiles, err := s.read.ListDashboardTilesByDashboardID(ctx, share.DashboardID)
	if err != nil {
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to list shared dashboard tiles", err,
			slog.String("share_id", shareID), slog.String("dashboard_id", share.DashboardID))
	}

	readShare := share
	return DashboardWithTiles{
		Dashboard: dashboard,
		Tiles:     tiles,
		Share:     &readShare,
	}, nil
}
