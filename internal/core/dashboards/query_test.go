package dashboards

import (
	"context"
	"strings"
	"testing"
	"time"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"google.golang.org/protobuf/proto"
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
	wantFrom := startOfDay(now.AddDate(0, 0, -6)) // LAST_7_DAYS = today + 6 prior days
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
	wantFrom := startOfDay(now.AddDate(0, 0, -29)) // LAST_30_DAYS fallback = today + 29 prior days
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
	rendered, err := RenderDashboard(context.Background(), nil, dashboard, DashboardQueryOverrides{})
	if err != nil {
		t.Fatalf("RenderDashboard returned error: %v", err)
	}

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

func TestRenderDashboard_PerTileRangeCapErrorIsolation(t *testing.T) {
	// Dashboard window GRANULARITY_MINUTE + LAST_30_DAYS violates the MINUTE 6h
	// range cap. The assembled per-tile QueryRequest fails re-validation, so the
	// insight tile surfaces an error_message while the markdown tile and the
	// overall render are unaffected. This guards the per-tile range-cap
	// re-validation path (query.go) that only runs because the window is attached
	// at render time.
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
	}
	queryJSON, err := SpecMessageToMap(spec)
	if err != nil {
		t.Fatalf("SpecMessageToMap: %v", err)
	}
	dashboard := DashboardWithTiles{
		Dashboard: dbread.Dashboard{ProjectID: "proj", DefaultTimeRange: "TIME_RANGE_PRESET_LAST_30_DAYS", DefaultGranularity: "GRANULARITY_MINUTE"},
		Tiles: []dbread.DashboardTile{
			{ID: "insight", Kind: int16(TileKindInsight), InsightQuery: queryJSON},
			{ID: "markdown", Kind: int16(TileKindMarkdown)},
		},
	}

	// nil executor is safe: the insight tile fails at protovalidate (range cap)
	// before ExecuteQuery is reached.
	rendered, err := RenderDashboard(context.Background(), nil, dashboard, DashboardQueryOverrides{})
	if err != nil {
		t.Fatalf("RenderDashboard returned error: %v", err)
	}
	if len(rendered.Tiles) != 2 {
		t.Fatalf("tiles = %d, want 2", len(rendered.Tiles))
	}

	insight := rendered.Tiles[0]
	if insight.Result != nil {
		t.Errorf("insight tile should have no result, got %v", insight.Result)
	}
	if !strings.Contains(insight.ErrorMessage, "invalid query parameters") {
		t.Errorf("insight tile error = %q, want range-cap validation error", insight.ErrorMessage)
	}

	markdown := rendered.Tiles[1]
	if markdown.Result != nil || markdown.ErrorMessage != "" {
		t.Errorf("markdown tile must have no outcome: result=%v err=%q", markdown.Result, markdown.ErrorMessage)
	}
}
