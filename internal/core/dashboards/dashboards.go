package dashboards

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rs/xid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

var (
	ErrDashboardNotFound                = errors.New("dashboard not found")
	ErrDashboardTileNotFound            = errors.New("dashboard tile not found")
	ErrDashboardTileDisplayNameConflict = errors.New("dashboard tile display name already in use")
)

// TileKind mirrors the proto oneof discriminator and the DB `kind` column.
type TileKind int16

const (
	TileKindInsight  TileKind = 1
	TileKindMarkdown TileKind = 2
)

// TileViewMode mirrors DashboardTileViewMode in the proto. The DB stores the
// corresponding proto enum name.
type TileViewMode int16

const (
	TileViewModeUnspecified TileViewMode = 0
	TileViewModeLine        TileViewMode = 1
	TileViewModeArea        TileViewMode = 2
	TileViewModeBarGrouped  TileViewMode = 3
	TileViewModeBarStacked  TileViewMode = 4
	TileViewModeTable       TileViewMode = 5
)

// DashboardDefaultTimeRange mirrors common.v1.TimeRangePreset in the proto. The DB
// stores the corresponding proto enum name.
type DashboardDefaultTimeRange int16

const (
	DashboardDefaultTimeRangeUnspecified DashboardDefaultTimeRange = 0
	DashboardDefaultTimeRangeLast1Hour   DashboardDefaultTimeRange = 1
	DashboardDefaultTimeRangeLast6Hours  DashboardDefaultTimeRange = 2
	DashboardDefaultTimeRangeLast24Hours DashboardDefaultTimeRange = 3
	DashboardDefaultTimeRangeLast7Days   DashboardDefaultTimeRange = 4
	DashboardDefaultTimeRangeLast14Days  DashboardDefaultTimeRange = 5
	DashboardDefaultTimeRangeLast30Days  DashboardDefaultTimeRange = 6
	DashboardDefaultTimeRangeLast90Days  DashboardDefaultTimeRange = 7
	DashboardDefaultTimeRangeLast180Days DashboardDefaultTimeRange = 8
	DashboardDefaultTimeRangeLast365Days DashboardDefaultTimeRange = 9
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

// DashboardWithTiles bundles a dashboard with its ordered tiles.
type DashboardWithTiles struct {
	Dashboard dbread.Dashboard
	Tiles     []dbread.DashboardTile
}

func (s *Service) CreateDashboard(ctx context.Context, projectID, displayName, description string, defaultTimeRange commonv1.TimeRangePreset, defaultGranularity insightsv1.Granularity) (dbwrite.Dashboard, error) {
	dashboard, err := s.write.CreateDashboard(ctx, dbwrite.CreateDashboardParams{
		Description:        description,
		ID:                 xid.New().String(),
		ProjectID:          projectID,
		DisplayName:        displayName,
		DefaultTimeRange:   dashboardDefaultTimeRangeDBName(normalizedDashboardDefaultTimeRange(defaultTimeRange)),
		DefaultGranularity: dashboardGranularityDBName(defaultGranularity),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to create dashboard",
			slogx.Error(err),
			slog.String("project_id", projectID),
		)
		telemetry.RecordError(ctx, err)
		return dbwrite.Dashboard{}, err
	}
	return dashboard, nil
}

func (s *Service) ListDashboards(ctx context.Context, projectID string) ([]DashboardWithTiles, error) {
	dashboards, err := s.read.ListDashboardsByProjectID(ctx, projectID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list dashboards",
			slogx.Error(err),
			slog.String("project_id", projectID),
		)
		telemetry.RecordError(ctx, err)
		return nil, err
	}

	tiles, err := s.read.ListDashboardTilesByProjectID(ctx, projectID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list dashboard tiles",
			slogx.Error(err),
			slog.String("project_id", projectID),
		)
		telemetry.RecordError(ctx, err)
		return nil, err
	}

	tilesByDashboardID := make(map[string][]dbread.DashboardTile, len(dashboards))
	for _, tile := range tiles {
		tilesByDashboardID[tile.DashboardID] = append(tilesByDashboardID[tile.DashboardID], tile)
	}

	result := make([]DashboardWithTiles, 0, len(dashboards))
	for _, dashboard := range dashboards {
		result = append(result, DashboardWithTiles{
			Dashboard: dashboard,
			Tiles:     tilesByDashboardID[dashboard.ID],
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
		slog.ErrorContext(ctx, "failed to get dashboard",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("dashboard_id", dashboardID),
		)
		telemetry.RecordError(ctx, err)
		return DashboardWithTiles{}, err
	}

	tiles, err := s.read.ListDashboardTilesByDashboardIDAndProjectID(ctx, dbread.ListDashboardTilesByDashboardIDAndProjectIDParams{
		DashboardID: dashboardID,
		ProjectID:   projectID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to list dashboard tiles",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("dashboard_id", dashboardID),
		)
		telemetry.RecordError(ctx, err)
		return DashboardWithTiles{}, err
	}

	return DashboardWithTiles{
		Dashboard: dashboard,
		Tiles:     tiles,
	}, nil
}

// UpdateDashboard updates the dashboard's display name, description, and
// dashboard-level window (default time range + granularity), and returns the
// updated row alongside the dashboard's existing tiles so the handler can
// serialize a complete Dashboard without a follow-up read. Description is
// updated with partial-update semantics (empty string preserves the existing
// value); display_name, default_time_range, and default_granularity are
// full-replaced on every call.
func (s *Service) UpdateDashboard(ctx context.Context, projectID, dashboardID, displayName, description string, defaultTimeRange commonv1.TimeRangePreset, defaultGranularity insightsv1.Granularity) (DashboardWithTiles, error) {
	dashboard, err := s.write.UpdateDashboard(ctx, dbwrite.UpdateDashboardParams{
		Description:        description,
		ID:                 dashboardID,
		ProjectID:          projectID,
		DisplayName:        displayName,
		DefaultTimeRange:   dashboardDefaultTimeRangeDBName(normalizedDashboardDefaultTimeRange(defaultTimeRange)),
		DefaultGranularity: dashboardGranularityDBName(defaultGranularity),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.DebugContext(ctx, "update dashboard display name: not found",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
			)
			return DashboardWithTiles{}, ErrDashboardNotFound
		}
		slog.ErrorContext(ctx, "failed to update dashboard display name",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("dashboard_id", dashboardID),
		)
		telemetry.RecordError(ctx, err)
		return DashboardWithTiles{}, err
	}

	tiles, err := s.read.ListDashboardTilesByDashboardIDAndProjectID(ctx, dbread.ListDashboardTilesByDashboardIDAndProjectIDParams{
		DashboardID: dashboardID,
		ProjectID:   projectID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to list dashboard tiles after rename",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("dashboard_id", dashboardID),
		)
		telemetry.RecordError(ctx, err)
		return DashboardWithTiles{}, err
	}

	return DashboardWithTiles{
		Dashboard: dbwriteToDbread(dashboard),
		Tiles:     tiles,
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
		slog.ErrorContext(ctx, "failed to delete dashboard",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("dashboard_id", dashboardID),
		)
		telemetry.RecordError(ctx, err)
		return err
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

func (s *Service) CreateDashboardTile(
	ctx context.Context,
	projectID, dashboardID, displayName, description string,
	content TileContent,
	viewMode dashboardsv1.DashboardTileViewMode,
	layouts []*dashboardsv1.ResponsiveGridLayout,
) (dbwrite.DashboardTile, error) {
	enc, err := content.Encode()
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode dashboard tile content",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("dashboard_id", dashboardID),
		)
		telemetry.RecordError(ctx, err)
		return dbwrite.DashboardTile{}, err
	}
	layoutsMap := LayoutsToMap(layouts)
	normalizedViewMode := normalizedTileViewMode(enc.Kind, viewMode)

	tile, err := s.write.CreateDashboardTile(ctx, dbwrite.CreateDashboardTileParams{
		ID:               xid.New().String(),
		DashboardID:      dashboardID,
		ProjectID:        projectID,
		Kind:             int16(enc.Kind),
		ViewMode:         tileViewModeDBName(normalizedViewMode),
		DisplayName:      displayName,
		Description:      description,
		InsightQuery:     enc.InsightQuery,
		MarkdownBody:     enc.MarkdownBody,
		Layouts:          layoutsMap,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.DebugContext(ctx, "create dashboard tile: dashboard not found",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
			)
			return dbwrite.DashboardTile{}, ErrDashboardNotFound
		}
		if conflict := translateUniqueViolation(err); conflict != nil {
			return dbwrite.DashboardTile{}, conflict
		}
		slog.ErrorContext(ctx, "failed to create dashboard tile",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("dashboard_id", dashboardID),
		)
		telemetry.RecordError(ctx, err)
		return dbwrite.DashboardTile{}, err
	}
	return tile, nil
}

func (s *Service) UpdateDashboardTile(
	ctx context.Context,
	projectID, dashboardID, tileID, displayName, description string,
	content TileContent,
	viewMode dashboardsv1.DashboardTileViewMode,
	layouts []*dashboardsv1.ResponsiveGridLayout,
) (dbwrite.DashboardTile, error) {
	enc, err := content.Encode()
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode dashboard tile content",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("dashboard_id", dashboardID),
			slog.String("tile_id", tileID),
		)
		telemetry.RecordError(ctx, err)
		return dbwrite.DashboardTile{}, err
	}
	layoutsMap := LayoutsToMap(layouts)
	normalizedViewMode := normalizedTileViewMode(enc.Kind, viewMode)

	tile, err := s.write.UpdateDashboardTile(ctx, dbwrite.UpdateDashboardTileParams{
		ID:               tileID,
		DashboardID:      dashboardID,
		ProjectID:        projectID,
		Kind:             int16(enc.Kind),
		ViewMode:         tileViewModeDBName(normalizedViewMode),
		DisplayName:      displayName,
		Description:      description,
		InsightQuery:     enc.InsightQuery,
		MarkdownBody:     enc.MarkdownBody,
		Layouts:          layoutsMap,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.DebugContext(ctx, "update dashboard tile: tile not found",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
				slog.String("tile_id", tileID),
			)
			return dbwrite.DashboardTile{}, ErrDashboardTileNotFound
		}
		if conflict := translateUniqueViolation(err); conflict != nil {
			return dbwrite.DashboardTile{}, conflict
		}
		slog.ErrorContext(ctx, "failed to update dashboard tile",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("dashboard_id", dashboardID),
			slog.String("tile_id", tileID),
		)
		telemetry.RecordError(ctx, err)
		return dbwrite.DashboardTile{}, err
	}
	return tile, nil
}

func (s *Service) DeleteDashboardTile(ctx context.Context, projectID, dashboardID, tileID string) error {
	if _, err := s.write.DeleteDashboardTile(ctx, dbwrite.DeleteDashboardTileParams{
		ID:          tileID,
		DashboardID: dashboardID,
		ProjectID:   projectID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.DebugContext(ctx, "delete dashboard tile: tile not found",
				slog.String("project_id", projectID),
				slog.String("dashboard_id", dashboardID),
				slog.String("tile_id", tileID),
			)
			return ErrDashboardTileNotFound
		}
		slog.ErrorContext(ctx, "failed to delete dashboard tile",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("dashboard_id", dashboardID),
			slog.String("tile_id", tileID),
		)
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// translateUniqueViolation returns ErrDashboardTileDisplayNameConflict if err is a
// Postgres unique-violation, or nil otherwise. The dashboard_tiles table has exactly
// one unique constraint (the partial display-name index), so any 23505 on it is the
// display-name conflict. Called from both CreateDashboardTile and UpdateDashboardTile
// error paths.
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

func normalizedTileViewMode(kind TileKind, viewMode dashboardsv1.DashboardTileViewMode) TileViewMode {
	switch kind {
	case TileKindInsight:
		switch viewMode {
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA:
			return TileViewModeArea
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED:
			return TileViewModeBarGrouped
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED:
			return TileViewModeBarStacked
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE:
			return TileViewModeTable
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE:
			return TileViewModeLine
		default:
			return TileViewModeLine
		}
	case TileKindMarkdown:
		return TileViewModeUnspecified
	default:
		return TileViewModeUnspecified
	}
}

// normalizedDashboardDefaultTimeRange maps a TimeRangePreset to its enum mirror,
// defaulting unknown/UNSPECIFIED to LAST_30_DAYS. Dashboard-level: no tile kind,
// no markdown coercion.
func normalizedDashboardDefaultTimeRange(defaultTimeRange commonv1.TimeRangePreset) DashboardDefaultTimeRange {
	switch defaultTimeRange {
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_1_HOUR:
		return DashboardDefaultTimeRangeLast1Hour
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_6_HOURS:
		return DashboardDefaultTimeRangeLast6Hours
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_24_HOURS:
		return DashboardDefaultTimeRangeLast24Hours
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS:
		return DashboardDefaultTimeRangeLast7Days
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS:
		return DashboardDefaultTimeRangeLast14Days
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS:
		return DashboardDefaultTimeRangeLast30Days
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS:
		return DashboardDefaultTimeRangeLast90Days
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS:
		return DashboardDefaultTimeRangeLast180Days
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_365_DAYS:
		return DashboardDefaultTimeRangeLast365Days
	default:
		return DashboardDefaultTimeRangeLast30Days
	}
}

func tileViewModeDBName(viewMode TileViewMode) string {
	switch viewMode {
	case TileViewModeLine:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE.String()
	case TileViewModeArea:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA.String()
	case TileViewModeBarGrouped:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED.String()
	case TileViewModeBarStacked:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED.String()
	case TileViewModeTable:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE.String()
	default:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED.String()
	}
}

func dashboardDefaultTimeRangeDBName(defaultTimeRange DashboardDefaultTimeRange) string {
	switch defaultTimeRange {
	case DashboardDefaultTimeRangeLast1Hour:
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_1_HOUR.String()
	case DashboardDefaultTimeRangeLast6Hours:
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_6_HOURS.String()
	case DashboardDefaultTimeRangeLast24Hours:
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_24_HOURS.String()
	case DashboardDefaultTimeRangeLast7Days:
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS.String()
	case DashboardDefaultTimeRangeLast14Days:
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS.String()
	case DashboardDefaultTimeRangeLast30Days:
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS.String()
	case DashboardDefaultTimeRangeLast90Days:
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS.String()
	case DashboardDefaultTimeRangeLast180Days:
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS.String()
	case DashboardDefaultTimeRangeLast365Days:
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_365_DAYS.String()
	default:
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED.String()
	}
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

func LayoutsToMap(layouts []*dashboardsv1.ResponsiveGridLayout) map[string]any {
	out := make(map[string]any, len(layouts))
	for _, layout := range layouts {
		out[layout.GetBreakpoint()] = map[string]any{
			"x":      layout.GetX(),
			"y":      layout.GetY(),
			"w":      layout.GetW(),
			"h":      layout.GetH(),
			"minW":   layout.GetMinW(),
			"maxW":   layout.GetMaxW(),
			"minH":   layout.GetMinH(),
			"maxH":   layout.GetMaxH(),
			"static": layout.GetStatic(),
		}
	}
	return out
}

func MapToLayouts(data map[string]any) ([]*dashboardsv1.ResponsiveGridLayout, error) {
	if len(data) == 0 {
		return nil, nil
	}

	breakpoints := make([]string, 0, len(data))
	for breakpoint := range data {
		breakpoints = append(breakpoints, breakpoint)
	}
	sort.Strings(breakpoints)

	out := make([]*dashboardsv1.ResponsiveGridLayout, 0, len(data))
	for _, breakpoint := range breakpoints {
		raw, err := json.Marshal(data[breakpoint])
		if err != nil {
			return nil, err
		}
		var item struct {
			X      int32 `json:"x"`
			Y      int32 `json:"y"`
			W      int32 `json:"w"`
			H      int32 `json:"h"`
			MinW   int32 `json:"minW"`
			MaxW   int32 `json:"maxW"`
			MinH   int32 `json:"minH"`
			MaxH   int32 `json:"maxH"`
			Static bool  `json:"static"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		out = append(out, &dashboardsv1.ResponsiveGridLayout{
			Breakpoint: proto.String(breakpoint),
			X:          proto.Int32(item.X),
			Y:          proto.Int32(item.Y),
			W:          proto.Int32(item.W),
			H:          proto.Int32(item.H),
			MinW:       proto.Int32(item.MinW),
			MaxW:       proto.Int32(item.MaxW),
			MinH:       proto.Int32(item.MinH),
			MaxH:       proto.Int32(item.MaxH),
			Static:     proto.Bool(item.Static),
		})
	}

	return out, nil
}
