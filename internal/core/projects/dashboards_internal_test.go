package projects

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
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

func TestNormalizedTileDefaultTimeRange_AllInsightPresets(t *testing.T) {
	cases := []struct {
		name string
		in   commonv1.TimeRangePreset
		want TileDefaultTimeRange
	}{
		{"last_1_hour", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_1_HOUR, TileDefaultTimeRangeLast1Hour},
		{"last_6_hours", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_6_HOURS, TileDefaultTimeRangeLast6Hours},
		{"last_24_hours", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_24_HOURS, TileDefaultTimeRangeLast24Hours},
		{"yesterday", commonv1.TimeRangePreset_TIME_RANGE_PRESET_YESTERDAY, TileDefaultTimeRangeYesterday},
		{"last_7_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS, TileDefaultTimeRangeLast7Days},
		{"last_14_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS, TileDefaultTimeRangeLast14Days},
		{"last_week", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_WEEK, TileDefaultTimeRangeLastWeek},
		{"last_month", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_MONTH, TileDefaultTimeRangeLastMonth},
		{"last_3_months", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_3_MONTHS, TileDefaultTimeRangeLast3Months},
		{"last_6_months", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_6_MONTHS, TileDefaultTimeRangeLast6Months},
		{"last_year", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_YEAR, TileDefaultTimeRangeLastYear},
		{"unspecified_defaults", commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED, TileDefaultTimeRangeLastMonth},
		{"unknown_defaults", commonv1.TimeRangePreset(99), TileDefaultTimeRangeLastMonth},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizedTileDefaultTimeRange(TileKindInsight, tc.in)
			if got != tc.want {
				t.Fatalf("normalizedTileDefaultTimeRange(insight, %v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
