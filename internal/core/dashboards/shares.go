package dashboards

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// newShareToken returns a 32-byte cryptographically-random token, hex-encoded
// (64 chars). It is the sole secret gating the unauthenticated
// SharedDashboardsService.Query, so it must come from crypto/rand — not a
// monotonic id generator like xid (mirrors auth.newActionToken).
func newShareToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// dbwriteShareToDbread bridges the independently-generated write/read structs
// for dashboard_shares (the columns match 1:1 but the Go types are distinct, per
// the sqlc read/write split). Keep the field list in sync when a column is
// added — a forgotten field compiles cleanly but silently zeroes the column on
// the write-return path. TestDbwriteShareToDbread_FieldParity pins the count.
func dbwriteShareToDbread(share dbwrite.DashboardShare) dbread.DashboardShare {
	return dbread.DashboardShare{
		ID:          share.ID,
		DashboardID: share.DashboardID,
		ProjectID:   share.ProjectID,
		ShareToken:  share.ShareToken,
		Enabled:     share.Enabled,
		CreateTime:  share.CreateTime,
		UpdateTime:  share.UpdateTime,
	}
}

// enabledShare establishes the DashboardWithTiles.Share invariant in exactly one
// place: it returns a non-nil pointer only for an ENABLED share, so every reader
// may treat Share != nil as "the dashboard is publicly shared". A disabled row
// collapses to nil. All producers funnel through here (lookupShare, setShare)
// so the rule cannot drift across call sites.
func enabledShare(share dbread.DashboardShare) *dbread.DashboardShare {
	if !share.Enabled {
		return nil
	}
	return &share
}

// lookupShare reads the share row for a dashboard through the given querier
// (so it composes with a transaction). Returns nil for no row or a disabled
// share; a non-nil result is always enabled (enabledShare).
func lookupShare(ctx context.Context, r *dbread.Queries, dashboardID string) (*dbread.DashboardShare, error) {
	share, err := r.GetDashboardShareByDashboardID(ctx, dashboardID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, recordServiceError(ctx, "failed to get dashboard share", err,
			slog.String("dashboard_id", dashboardID))
	}
	return enabledShare(share), nil
}

// setShare upserts the share row for a dashboard, toggling enabled without
// rotating the id or share token on re-enable. The freshly-minted id and
// crypto-random token are used only on the first INSERT; ON CONFLICT preserves
// the existing ones (see UpsertDashboardShare), so a public link survives a
// disable/re-enable cycle.
func setShare(ctx context.Context, w *dbwrite.Queries, projectID, dashboardID string, enabled bool) (*dbread.DashboardShare, error) {
	token, err := newShareToken()
	if err != nil {
		return nil, recordServiceError(ctx, "failed to generate share token", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}
	share, err := w.UpsertDashboardShare(ctx, dbwrite.UpsertDashboardShareParams{
		ID:          xid.New().String(),
		DashboardID: dashboardID,
		ProjectID:   projectID,
		ShareToken:  token,
		Enabled:     enabled,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.DebugContext(ctx, "set dashboard share: dashboard not found",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
			)
			return nil, ErrDashboardNotFound
		}
		return nil, recordServiceError(ctx, "failed to set dashboard share", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}
	return enabledShare(dbwriteShareToDbread(share)), nil
}

// GetSharedDashboard loads a dashboard and its tiles via an enabled share token.
// The token is the public capability secret, so it is never written to logs or
// telemetry attributes; diagnostics key on the resolved dashboard_id instead.
func (s *Service) GetSharedDashboard(ctx context.Context, shareToken string) (DashboardWithTiles, error) {
	share, err := s.read.GetEnabledDashboardShareByToken(ctx, shareToken)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.DebugContext(ctx, "get shared dashboard: share token not found or disabled")
			return DashboardWithTiles{}, ErrDashboardNotFound
		}
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to get dashboard share by token", err)
	}

	dashboard, err := s.read.GetDashboardByIDAndProjectID(ctx, dbread.GetDashboardByIDAndProjectIDParams{
		ID:        share.DashboardID,
		ProjectID: share.ProjectID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The FK is ON DELETE CASCADE, so a share outliving its dashboard is an
			// impossible state under normal operation. Record it as a data-integrity
			// signal (matching the degraded-tile sites in rpc.go), not just a warning.
			slog.WarnContext(ctx, "get shared dashboard: share references missing dashboard",
				slog.String("dashboard_id", share.DashboardID),
			)
			telemetry.RecordError(ctx, fmt.Errorf("dashboard share references missing dashboard %s", share.DashboardID))
			return DashboardWithTiles{}, ErrDashboardNotFound
		}
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to get shared dashboard", err,
			slog.String("dashboard_id", share.DashboardID))
	}

	tiles, err := s.read.ListDashboardTilesByDashboardID(ctx, share.DashboardID)
	if err != nil {
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to list shared dashboard tiles", err,
			slog.String("dashboard_id", share.DashboardID))
	}

	readShare := share
	return DashboardWithTiles{
		Dashboard: dashboard,
		Tiles:     tiles,
		Share:     &readShare,
	}, nil
}
