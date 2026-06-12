package dashboards

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rs/xid"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

var (
	ErrDashboardNotFound                = errors.New("dashboard not found")
	ErrDashboardTileNotFound            = errors.New("dashboard tile not found")
	ErrDashboardTileDisplayNameConflict = errors.New("dashboard tile display name already in use")
	ErrDuplicateUpsertTileID            = errors.New("duplicate tile id in upsert request")
)

// TileKind mirrors the proto oneof discriminator and the DB `kind` column.
type TileKind int16

const (
	TileKindInsight  TileKind = 1
	TileKindMarkdown TileKind = 2
)

// TileContent is a sealed sum type for tile payloads. Encode() returns the
// DB-shaped tuple; every variant must implement it, so adding a new variant
// without wiring it into the storage layer is a compile error rather than
// a runtime panic. The handler layer translates the proto oneof to one of
// the concrete variants below.
type TileContent interface {
	isTileContent()
	Encode() (EncodedTileContent, error)
}

// InsightTile is the insight-kind variant of TileContent. Spec is a shared
// pointer; callers must not mutate it after constructing the tile — Encode
// snapshots it via protojson before any DB write. The time window and
// granularity live on the dashboard, not the tile.
type InsightTile struct {
	Spec *insightsv1.InsightQuerySpec
}

// MarkdownTile is the markdown-kind variant of TileContent. Empty Body is
// allowed by this type but rejected by proto validation (min_len: 1).
type MarkdownTile struct {
	Body string
}

func (InsightTile) isTileContent()  {}
func (MarkdownTile) isTileContent() {}

// Encode translates the insight payload to the DB-shaped tuple. The jsonb
// column is map[string]any per sqlc.yaml; nil map maps to SQL NULL via pgx.
func (i InsightTile) Encode() (EncodedTileContent, error) {
	queryJSON, err := SpecMessageToMap(i.Spec)
	if err != nil {
		return EncodedTileContent{}, err
	}
	return EncodedTileContent{Kind: TileKindInsight, InsightQuery: queryJSON}, nil
}

// Encode translates the markdown payload to the DB-shaped tuple. Empty body
// is preserved verbatim — pgtype.Text{Valid: true, String: ""} is distinct
// from SQL NULL, which is what the CHECK constraint requires for kind = 2.
func (m MarkdownTile) Encode() (EncodedTileContent, error) {
	return EncodedTileContent{Kind: TileKindMarkdown, MarkdownBody: pgtype.Text{String: m.Body, Valid: true}}, nil
}

// EncodedTileContent is the DB-shaped result of TileContent.Encode: the
// (kind, insight_query, markdown_body) tuple that satisfies the
// dashboard_tiles_kind_payload CHECK constraint.
type EncodedTileContent struct {
	Kind         TileKind
	InsightQuery map[string]any
	MarkdownBody pgtype.Text
}

// DashboardWithTiles bundles a dashboard with its ordered tiles and optional
// share. Invariant: Share is non-nil only when the dashboard is publicly shared
// (the share row is enabled); a nil Share means private — never shared OR sharing
// disabled. Producers must establish this via enabledShare, never by attaching a
// raw share row, because the RPC encoder treats Share != nil as "shared".
type DashboardWithTiles struct {
	Dashboard dbread.Dashboard
	Tiles     []dbread.DashboardTile
	Share     *dbread.DashboardShare
}

// recordServiceError logs + records a service-layer error at the layer that detects
// it (per telemetry.md), then returns it unchanged so handlers map the status. It
// is the single chokepoint for the service's DB error paths. A client context
// cancellation/deadline is returned unchanged but NOT logged or recorded — a
// disconnected or timed-out caller would otherwise manufacture error-rate noise
// (mirrors the insights executor's recordQueryError).
func recordServiceError(ctx context.Context, msg string, err error, attrs ...any) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	slog.ErrorContext(ctx, msg, append([]any{slogx.Error(err)}, attrs...)...)
	telemetry.RecordError(ctx, err)
	return err
}

// CreateDashboard persists a dashboard with its display fields and dashboard-level
// window. The (default_time_range, default_granularity) pair is normalized but NOT
// checked for satisfiability against the per-granularity range caps: an
// unsatisfiable pair (e.g. GRANULARITY_MINUTE + LAST_365_DAYS) is surfaced as each
// tile's error_message at render time (QueryDashboard re-validates the assembled
// per-tile QueryRequest), not rejected here. The caps live in proto CEL on
// QueryRequest; re-deriving them at write time would duplicate that rule and risk
// drift, so the window pairing is the client's responsibility.
func (s *Service) CreateDashboard(ctx context.Context, projectID, displayName, description string, defaultTimeRange commonv1.TimeRangePreset, defaultGranularity insightsv1.Granularity) (dbwrite.Dashboard, error) {
	dashboard, err := s.write.CreateDashboard(ctx, dbwrite.CreateDashboardParams{
		Description:        description,
		ID:                 xid.New().String(),
		ProjectID:          projectID,
		DisplayName:        displayName,
		DefaultTimeRange:   dashboardDefaultTimeRangeDBName(defaultTimeRange),
		DefaultGranularity: dashboardGranularityDBName(defaultGranularity),
	})
	if err != nil {
		return dbwrite.Dashboard{}, recordServiceError(ctx, "failed to create dashboard", err,
			slog.String("project_id", projectID))
	}
	return dashboard, nil
}

func (s *Service) ListDashboards(ctx context.Context, projectID string) ([]DashboardWithTiles, error) {
	dashboards, err := s.read.ListDashboardsByProjectID(ctx, projectID)
	if err != nil {
		return nil, recordServiceError(ctx, "failed to list dashboards", err,
			slog.String("project_id", projectID))
	}

	tiles, err := s.read.ListDashboardTilesByProjectID(ctx, projectID)
	if err != nil {
		return nil, recordServiceError(ctx, "failed to list dashboard tiles", err,
			slog.String("project_id", projectID))
	}

	tilesByDashboardID := make(map[string][]dbread.DashboardTile, len(dashboards))
	for _, tile := range tiles {
		tilesByDashboardID[tile.DashboardID] = append(tilesByDashboardID[tile.DashboardID], tile)
	}

	// Batch-load enabled shares so List reports share_id consistently with Get.
	// The query returns only enabled rows, so each entry satisfies the
	// DashboardWithTiles.Share invariant (non-nil ⇒ shared).
	shares, err := s.read.ListEnabledDashboardSharesByProjectID(ctx, projectID)
	if err != nil {
		return nil, recordServiceError(ctx, "failed to list dashboard shares", err,
			slog.String("project_id", projectID))
	}
	shareByDashboardID := make(map[string]*dbread.DashboardShare, len(shares))
	for i := range shares {
		shareByDashboardID[shares[i].DashboardID] = &shares[i]
	}

	result := make([]DashboardWithTiles, 0, len(dashboards))
	for _, dashboard := range dashboards {
		result = append(result, DashboardWithTiles{
			Dashboard: dashboard,
			Tiles:     tilesByDashboardID[dashboard.ID],
			Share:     shareByDashboardID[dashboard.ID],
		})
	}

	return result, nil
}

func (s *Service) GetDashboard(ctx context.Context, projectID, dashboardID string) (DashboardWithTiles, error) {
	dashboard, err := s.read.GetDashboardByIDAndProjectID(ctx, dbread.GetDashboardByIDAndProjectIDParams{
		ID:        dashboardID,
		ProjectID: projectID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.DebugContext(ctx, "get dashboard: not found",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
			)
			return DashboardWithTiles{}, ErrDashboardNotFound
		}
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to get dashboard", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}

	tiles, err := s.read.ListDashboardTilesByDashboardIDAndProjectID(ctx, dbread.ListDashboardTilesByDashboardIDAndProjectIDParams{
		DashboardID: dashboardID,
		ProjectID:   projectID,
	})
	if err != nil {
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to list dashboard tiles", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}

	// The share is supplementary metadata; a transient dashboard_shares read
	// failure must not fail the whole dashboard read. lookupShare has already
	// logged+recorded any real error, so degrade to a private view (Share = nil)
	// — except for client cancellation/deadline, which should surface.
	share, err := lookupShare(ctx, s.read, dashboardID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return DashboardWithTiles{}, err
		}
		share = nil
	}

	return DashboardWithTiles{
		Dashboard: dashboard,
		Tiles:     tiles,
		Share:     share,
	}, nil
}

// UpdateDashboardInput is the service-layer projection of
// DashboardsServiceUpdateRequest. The handler builds this from the proto.
// IsPublic carries proto field presence: nil means "leave public-sharing state
// untouched" (the partial-update semantics description also uses), while a
// non-nil value full-replaces the share's enabled flag.
type UpdateDashboardInput struct {
	DisplayName        string
	Description        string
	DefaultTimeRange   commonv1.TimeRangePreset
	DefaultGranularity insightsv1.Granularity
	IsPublic           *bool
}

// UpdateDashboard updates the dashboard's display name, description, and
// dashboard-level window (default time range + granularity), and optionally
// toggles public sharing. Returns the updated row alongside the dashboard's
// existing tiles so the handler can serialize a complete Dashboard without a
// follow-up read. Description is updated with partial-update semantics (empty
// string preserves the existing value); display_name, default_time_range, and
// default_granularity are full-replaced on every call. Public sharing is
// presence-aware: in.IsPublic == nil leaves the share untouched (the response
// reflects the current share), while a non-nil value enables/disables it.
func (s *Service) UpdateDashboard(ctx context.Context, projectID, dashboardID string, in UpdateDashboardInput) (result DashboardWithTiles, retErr error) {
	// One transaction so the metadata write and the share toggle commit (or roll
	// back) together — consistent with the Upsert path's atomicity. Without it, a
	// share-write failure after the metadata write lands would leave the rename
	// applied but the toggle dropped while the client sees an error.
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to begin update transaction", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			attrs := []any{slogx.Error(rbErr),
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID)}
			if retErr != nil {
				attrs = append(attrs, slog.String("triggering_error", retErr.Error()))
			}
			slog.WarnContext(ctx, "failed to rollback update transaction", attrs...)
		}
	}()
	writeTx := s.write.WithTx(tx)
	readTx := s.read.WithTx(tx)

	dashboard, err := writeTx.UpdateDashboard(ctx, dbwrite.UpdateDashboardParams{
		Description:        in.Description,
		ID:                 dashboardID,
		ProjectID:          projectID,
		DisplayName:        in.DisplayName,
		DefaultTimeRange:   dashboardDefaultTimeRangeDBName(in.DefaultTimeRange),
		DefaultGranularity: dashboardGranularityDBName(in.DefaultGranularity),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.DebugContext(ctx, "update dashboard: not found",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
			)
			return DashboardWithTiles{}, ErrDashboardNotFound
		}
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to update dashboard", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}

	tiles, err := readTx.ListDashboardTilesByDashboardIDAndProjectID(ctx, dbread.ListDashboardTilesByDashboardIDAndProjectIDParams{
		DashboardID: dashboardID,
		ProjectID:   projectID,
	})
	if err != nil {
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to list dashboard tiles after update", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}

	// Presence-aware sharing: only touch the share row when is_public was sent.
	// On omission, reflect the existing share so the response's share_id stays
	// accurate without the caller having to re-send the current state. Both
	// helpers return a non-nil share only when it is enabled (see enabledShare).
	var sharePtr *dbread.DashboardShare
	if in.IsPublic != nil {
		sharePtr, err = setShare(ctx, writeTx, projectID, dashboardID, *in.IsPublic)
	} else {
		sharePtr, err = lookupShare(ctx, readTx, dashboardID)
	}
	if err != nil {
		return DashboardWithTiles{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return DashboardWithTiles{}, recordServiceError(ctx, "failed to commit update transaction", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}

	return DashboardWithTiles{
		Dashboard: dbwriteToDbread(dashboard),
		Tiles:     tiles,
		Share:     sharePtr,
	}, nil
}

func (s *Service) DeleteDashboard(ctx context.Context, projectID, dashboardID string) error {
	if _, err := s.write.DeleteDashboard(ctx, dbwrite.DeleteDashboardParams{
		ID:        dashboardID,
		ProjectID: projectID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.DebugContext(ctx, "delete dashboard: not found",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
			)
			return ErrDashboardNotFound
		}
		return recordServiceError(ctx, "failed to delete dashboard", err,
			slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID))
	}
	return nil
}

// dbwriteToDbread copies the dashboard row from the write-side struct to the
// read-side struct so a freshly-updated dashboard can be returned through
// the same DashboardWithTiles shape Get / List use. The two structs are
// generated independently from the same underlying table; the columns match
// 1:1 but the Go types are distinct.
func dbwriteToDbread(d dbwrite.Dashboard) dbread.Dashboard {
	return dbread.Dashboard{
		ID:                 d.ID,
		ProjectID:          d.ProjectID,
		DisplayName:        d.DisplayName,
		Description:        d.Description,
		DefaultTimeRange:   d.DefaultTimeRange,
		DefaultGranularity: d.DefaultGranularity,
		CreateTime:         d.CreateTime,
		UpdateTime:         d.UpdateTime,
	}
}

// translateUniqueViolation returns ErrDashboardTileDisplayNameConflict if err is a
// Postgres unique-violation, or nil otherwise. The dashboard_tiles table has exactly
// one unique constraint (the partial display-name index), so any 23505 on it is the
// display-name conflict. Called from the Upsert flow's insert/update branches.
func translateUniqueViolation(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}
	if pgErr.Code != pgerrcode.UniqueViolation {
		return nil
	}
	return ErrDashboardTileDisplayNameConflict
}

func SpecMessageToMap(msg *insightsv1.InsightQuerySpec) (map[string]any, error) {
	if msg == nil {
		return map[string]any{}, nil
	}
	data, err := protojson.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func MapToSpecMessage(data map[string]any) (*insightsv1.InsightQuerySpec, error) {
	if len(data) == 0 {
		return &insightsv1.InsightQuerySpec{}, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var out insightsv1.InsightQuerySpec
	if err := protojson.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// normalizedDashboardDefaultTimeRange returns the preset unchanged for a known
// value, defaulting unknown/UNSPECIFIED to LAST_30_DAYS. Mirrors
// normalizedDashboardGranularity — no parallel enum, so nothing to keep in sync.
func normalizedDashboardDefaultTimeRange(tr commonv1.TimeRangePreset) commonv1.TimeRangePreset {
	switch tr {
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_1_HOUR,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_6_HOURS,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_24_HOURS,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_365_DAYS:
		return tr
	default:
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS
	}
}

// dashboardDefaultTimeRangeDBName stores the preset as its proto enum name,
// normalizing unknown/UNSPECIFIED to LAST_30_DAYS. Self-normalizing and thus
// safe to call directly — mirrors dashboardGranularityDBName.
func dashboardDefaultTimeRangeDBName(tr commonv1.TimeRangePreset) string {
	return normalizedDashboardDefaultTimeRange(tr).String()
}

// normalizedDashboardGranularity defaults unknown/UNSPECIFIED granularity to DAY.
func normalizedDashboardGranularity(g insightsv1.Granularity) insightsv1.Granularity {
	switch g {
	case insightsv1.Granularity_GRANULARITY_MINUTE,
		insightsv1.Granularity_GRANULARITY_HOUR,
		insightsv1.Granularity_GRANULARITY_DAY,
		insightsv1.Granularity_GRANULARITY_WEEK,
		insightsv1.Granularity_GRANULARITY_MONTH:
		return g
	default:
		return insightsv1.Granularity_GRANULARITY_DAY
	}
}

// dashboardGranularityDBName stores the granularity as its proto enum name.
func dashboardGranularityDBName(g insightsv1.Granularity) string {
	return normalizedDashboardGranularity(g).String()
}
