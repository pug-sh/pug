package dashboards

import (
	"context"
	"testing"
	"time"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDashboardDefaultTimeRangePresetFromDB(t *testing.T) {
	if got := DashboardDefaultTimeRangePresetFromDB("TIME_RANGE_PRESET_LAST_7_DAYS"); got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS {
		t.Fatalf("got %v, want LAST_7_DAYS", got)
	}
	if got := DashboardDefaultTimeRangePresetFromDB("unknown"); got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS {
		t.Fatalf("unknown got %v, want LAST_30_DAYS", got)
	}
	if got := DashboardDefaultTimeRangePresetFromDB(""); got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS {
		t.Fatalf("empty got %v, want LAST_30_DAYS", got)
	}
	if got := DashboardDefaultTimeRangePresetFromDB("TIME_RANGE_PRESET_UNSPECIFIED"); got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS {
		t.Fatalf("unspecified got %v, want LAST_30_DAYS", got)
	}
}

func TestDashboardGranularityFromDB(t *testing.T) {
	if got := DashboardGranularityFromDB("GRANULARITY_WEEK"); got != insightsv1.Granularity_GRANULARITY_WEEK {
		t.Fatalf("got %v, want WEEK", got)
	}
	if got := DashboardGranularityFromDB("unknown"); got != insightsv1.Granularity_GRANULARITY_DAY {
		t.Fatalf("unknown got %v, want DAY", got)
	}
	if got := DashboardGranularityFromDB(""); got != insightsv1.Granularity_GRANULARITY_DAY {
		t.Fatalf("empty got %v, want DAY", got)
	}
}

func TestValidAbsoluteTimeRange(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	if validAbsoluteTimeRange(nil) {
		t.Fatal("nil range should be invalid")
	}
	if validAbsoluteTimeRange(&commonv1.TimeRange{
		From: timestamppb.New(now),
		To:   timestamppb.New(now),
	}) {
		t.Fatal("equal bounds should be invalid")
	}
	if !validAbsoluteTimeRange(&commonv1.TimeRange{
		From: timestamppb.New(now.Add(-time.Hour)),
		To:   timestamppb.New(now),
	}) {
		t.Fatal("expected valid range")
	}
}

func TestResolveDashboardTimeRangePreset(t *testing.T) {
	now := time.Date(2026, 5, 23, 15, 30, 0, 0, time.UTC)

	got := ResolveDashboardTimeRangePreset(commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS, nil, now)
	if got.GetFrom().AsTime().After(got.GetTo().AsTime()) {
		t.Fatal("expected from before to")
	}
	if got.GetTo().AsTime() != now {
		t.Fatalf("to = %v, want %v", got.GetTo().AsTime(), now)
	}

	fallback := &commonv1.TimeRange{
		From: timestamppb.New(now.Add(-2 * time.Hour)),
		To:   timestamppb.New(now.Add(-time.Hour)),
	}
	got = ResolveDashboardTimeRangePreset(commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED, fallback, now)
	if !got.GetFrom().AsTime().Equal(fallback.GetFrom().AsTime()) || !got.GetTo().AsTime().Equal(fallback.GetTo().AsTime()) {
		t.Fatalf("got fallback range %v-%v, want %v-%v", got.GetFrom().AsTime(), got.GetTo().AsTime(), fallback.GetFrom().AsTime(), fallback.GetTo().AsTime())
	}
}

func TestResolveEffectiveWindow_OverrideWins(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	override := &commonv1.TimeRange{From: timestamppb.New(now.Add(-2 * time.Hour)), To: timestamppb.New(now)}
	dash := dbread.Dashboard{DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_WEEK"}

	tr, gran := resolveEffectiveWindow(dash, DashboardQueryOverrides{TimeRange: override, Granularity: insightsv1.Granularity_GRANULARITY_HOUR}, now)
	if tr != override {
		t.Fatal("expected override time range to win")
	}
	if gran != insightsv1.Granularity_GRANULARITY_HOUR {
		t.Fatalf("granularity = %v, want HOUR", gran)
	}
}

func TestResolveEffectiveWindow_FallsBackToDashboardDefault(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	dash := dbread.Dashboard{DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_WEEK"}

	tr, gran := resolveEffectiveWindow(dash, DashboardQueryOverrides{}, now)
	if gran != insightsv1.Granularity_GRANULARITY_WEEK {
		t.Fatalf("granularity = %v, want WEEK", gran)
	}
	wantFrom := startOfDay(now.AddDate(0, 0, -7))
	if !tr.GetFrom().AsTime().Equal(wantFrom) {
		t.Fatalf("from = %v, want %v", tr.GetFrom().AsTime(), wantFrom)
	}
	if !tr.GetTo().AsTime().Equal(now) {
		t.Fatalf("to = %v, want %v", tr.GetTo().AsTime(), now)
	}
}

func TestResolveEffectiveWindow_UnknownDefaultsNormalize(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	dash := dbread.Dashboard{DefaultTimeRange: "", DefaultGranularity: ""}

	tr, gran := resolveEffectiveWindow(dash, DashboardQueryOverrides{}, now)
	if gran != insightsv1.Granularity_GRANULARITY_DAY {
		t.Fatalf("granularity = %v, want DAY", gran)
	}
	wantFrom := startOfDay(now.AddDate(0, 0, -30)) // LAST_30_DAYS fallback
	if !tr.GetFrom().AsTime().Equal(wantFrom) {
		t.Fatalf("from = %v, want %v (LAST_30_DAYS)", tr.GetFrom().AsTime(), wantFrom)
	}
}

func TestRenderDashboard_PreservesOrderAndIncludesMarkdown(t *testing.T) {
	dashboard := DashboardWithTiles{
		Dashboard: dbread.Dashboard{ProjectID: "proj", DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_DAY"},
		Tiles: []dbread.DashboardTile{
			{ID: "markdown", Kind: int16(TileKindMarkdown)},
			{ID: "insight-a", Kind: int16(TileKindInsight)},
			{ID: "insight-b", Kind: int16(TileKindInsight)},
		},
	}

	// nil executor is safe: insight tiles have empty InsightQuery, so each fails
	// before reaching ExecuteQuery.
	rendered := RenderDashboard(context.Background(), nil, dashboard, DashboardQueryOverrides{})

	if len(rendered.Tiles) != 3 {
		t.Fatalf("tiles = %d, want 3 (markdown included)", len(rendered.Tiles))
	}
	gotOrder := []string{rendered.Tiles[0].Tile.ID, rendered.Tiles[1].Tile.ID, rendered.Tiles[2].Tile.ID}
	wantOrder := []string{"markdown", "insight-a", "insight-b"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("tile order = %v, want %v", gotOrder, wantOrder)
		}
	}
	if rendered.Tiles[0].Result != nil || rendered.Tiles[0].ErrorMessage != "" {
		t.Fatalf("markdown tile must have no outcome: result=%v err=%q", rendered.Tiles[0].Result, rendered.Tiles[0].ErrorMessage)
	}
	for _, idx := range []int{1, 2} {
		if rendered.Tiles[idx].ErrorMessage == "" {
			t.Fatalf("insight tile %q expected per-tile error message", rendered.Tiles[idx].Tile.ID)
		}
	}
}
