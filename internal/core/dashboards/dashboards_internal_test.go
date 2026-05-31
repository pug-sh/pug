package dashboards

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
)

func TestTranslateUniqueViolation(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantSet bool // true => expect ErrDashboardTileDisplayNameConflict, false => expect nil
	}{
		{
			name:    "nil error",
			err:     nil,
			wantSet: false,
		},
		{
			name:    "plain non-pgconn error",
			err:     errors.New("boom"),
			wantSet: false,
		},
		{
			name:    "pgconn check-violation (different code)",
			err:     &pgconn.PgError{Code: pgerrcode.CheckViolation},
			wantSet: false,
		},
		{
			name:    "pgconn foreign-key-violation (different code)",
			err:     &pgconn.PgError{Code: pgerrcode.ForeignKeyViolation},
			wantSet: false,
		},
		{
			name:    "pgconn unique-violation (the only match)",
			err:     &pgconn.PgError{Code: pgerrcode.UniqueViolation},
			wantSet: true,
		},
		{
			name:    "wrapped pgconn unique-violation",
			err:     fmt.Errorf("write tile: %w", &pgconn.PgError{Code: pgerrcode.UniqueViolation}),
			wantSet: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateUniqueViolation(tc.err)
			if tc.wantSet {
				if !errors.Is(got, ErrDashboardTileDisplayNameConflict) {
					t.Errorf("translateUniqueViolation(%v) = %v, want ErrDashboardTileDisplayNameConflict", tc.err, got)
				}
			} else {
				if got != nil {
					t.Errorf("translateUniqueViolation(%v) = %v, want nil", tc.err, got)
				}
			}
		})
	}
}

func TestNormalizedDashboardDefaultTimeRange_AllPresets(t *testing.T) {
	cases := []struct {
		name string
		in   commonv1.TimeRangePreset
		want commonv1.TimeRangePreset
	}{
		{"last_1_hour", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_1_HOUR, commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_1_HOUR},
		{"last_6_hours", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_6_HOURS, commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_6_HOURS},
		{"last_24_hours", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_24_HOURS, commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_24_HOURS},
		{"last_7_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS, commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS},
		{"last_14_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS, commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS},
		{"last_30_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS},
		{"last_90_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS, commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS},
		{"last_180_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS, commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS},
		{"last_365_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_365_DAYS, commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_365_DAYS},
		{"unspecified_defaults", commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED, commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS},
		{"unknown_defaults", commonv1.TimeRangePreset(99), commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizedDashboardDefaultTimeRange(tc.in); got != tc.want {
				t.Fatalf("normalizedDashboardDefaultTimeRange(%v) = %v, want %v", tc.in, got, tc.want)
			}
			// DB name must round-trip to the normalized preset's enum name.
			if got := dashboardDefaultTimeRangeDBName(tc.in); got != tc.want.String() {
				t.Fatalf("dashboardDefaultTimeRangeDBName(%v) = %q, want %q", tc.in, got, tc.want.String())
			}
		})
	}
}

func TestNormalizedTileViewModeProto(t *testing.T) {
	cases := []struct {
		name string
		kind TileKind
		in   dashboardsv1.DashboardTileViewMode
		want dashboardsv1.DashboardTileViewMode
	}{
		{"insight_line", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE},
		{"insight_area", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA},
		{"insight_bar_grouped", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED},
		{"insight_bar_stacked", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED},
		{"insight_table", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE},
		{"insight_kpi", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI},
		{"insight_sankey", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_SANKEY, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_SANKEY},
		{"insight_unspecified_defaults_line", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE},
		{"insight_unknown_defaults_line", TileKindInsight, dashboardsv1.DashboardTileViewMode(99), dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE},
		{"markdown_coerces_unspecified", TileKindMarkdown, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED},
		{"markdown_unspecified", TileKindMarkdown, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizedTileViewModeProto(tc.kind, tc.in)
			if got != tc.want {
				t.Fatalf("normalizedTileViewModeProto(%v, %v) = %v, want %v", tc.kind, tc.in, got, tc.want)
			}
		})
	}
}
