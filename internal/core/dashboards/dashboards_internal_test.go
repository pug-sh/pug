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
		want DashboardDefaultTimeRange
	}{
		{"last_1_hour", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_1_HOUR, DashboardDefaultTimeRangeLast1Hour},
		{"last_6_hours", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_6_HOURS, DashboardDefaultTimeRangeLast6Hours},
		{"last_24_hours", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_24_HOURS, DashboardDefaultTimeRangeLast24Hours},
		{"last_7_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS, DashboardDefaultTimeRangeLast7Days},
		{"last_14_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS, DashboardDefaultTimeRangeLast14Days},
		{"last_30_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, DashboardDefaultTimeRangeLast30Days},
		{"last_90_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS, DashboardDefaultTimeRangeLast90Days},
		{"last_180_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS, DashboardDefaultTimeRangeLast180Days},
		{"last_365_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_365_DAYS, DashboardDefaultTimeRangeLast365Days},
		{"unspecified_defaults", commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED, DashboardDefaultTimeRangeLast30Days},
		{"unknown_defaults", commonv1.TimeRangePreset(99), DashboardDefaultTimeRangeLast30Days},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizedDashboardDefaultTimeRange(tc.in)
			if got != tc.want {
				t.Fatalf("normalizedDashboardDefaultTimeRange(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizedTileViewMode(t *testing.T) {
	cases := []struct {
		name string
		kind TileKind
		in   dashboardsv1.DashboardTileViewMode
		want TileViewMode
	}{
		{"insight_line", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE, TileViewModeLine},
		{"insight_area", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA, TileViewModeArea},
		{"insight_bar_grouped", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED, TileViewModeBarGrouped},
		{"insight_bar_stacked", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED, TileViewModeBarStacked},
		{"insight_table", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE, TileViewModeTable},
		{"insight_unspecified_defaults_line", TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED, TileViewModeLine},
		{"insight_unknown_defaults_line", TileKindInsight, dashboardsv1.DashboardTileViewMode(99), TileViewModeLine},
		{"markdown_coerces_unspecified", TileKindMarkdown, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED, TileViewModeUnspecified},
		{"markdown_unspecified", TileKindMarkdown, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED, TileViewModeUnspecified},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizedTileViewMode(tc.kind, tc.in)
			if got != tc.want {
				t.Fatalf("normalizedTileViewMode(%v, %v) = %d, want %d", tc.kind, tc.in, got, tc.want)
			}
		})
	}
}
