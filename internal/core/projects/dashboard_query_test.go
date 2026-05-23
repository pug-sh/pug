package projects

import (
	"context"
	"testing"
	"time"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTileDefaultTimeRangePresetFromDB(t *testing.T) {
	if got := TileDefaultTimeRangePresetFromDB(TileKindInsight, "TIME_RANGE_PRESET_LAST_7_DAYS"); got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS {
		t.Fatalf("got %v, want LAST_7_DAYS", got)
	}
	if got := TileDefaultTimeRangePresetFromDB(TileKindInsight, "unknown"); got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS {
		t.Fatalf("got %v, want LAST_30_DAYS", got)
	}
	if got := TileDefaultTimeRangePresetFromDB(TileKindMarkdown, "TIME_RANGE_PRESET_LAST_7_DAYS"); got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED {
		t.Fatalf("got %v, want UNSPECIFIED", got)
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

func TestBuildEffectiveTileQuery(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	storedFrom := now.Add(-48 * time.Hour)
	storedTo := now.Add(-24 * time.Hour)
	stored := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Granularity: insightsv1.Granularity_GRANULARITY_WEEK.Enum(),
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(storedFrom),
			To:   timestamppb.New(storedTo),
		},
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("signup")}},
		},
	}

	overrideFrom := now.Add(-6 * time.Hour)
	overrideTo := now
	overrideRange := &commonv1.TimeRange{
		From: timestamppb.New(overrideFrom),
		To:   timestamppb.New(overrideTo),
	}

	effective, err := buildEffectiveTileQuery(
		stored,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS,
		DashboardQueryOverrides{
			TimeRange:   overrideRange,
			Granularity: insightsv1.Granularity_GRANULARITY_HOUR,
		},
		now,
	)
	if err != nil {
		t.Fatalf("buildEffectiveTileQuery: %v", err)
	}
	if !effective.GetTimeRange().GetFrom().AsTime().Equal(overrideFrom) {
		t.Fatalf("time range from = %v, want %v", effective.GetTimeRange().GetFrom().AsTime(), overrideFrom)
	}
	if effective.GetGranularity() != insightsv1.Granularity_GRANULARITY_HOUR {
		t.Fatalf("granularity = %v, want HOUR", effective.GetGranularity())
	}

	effective, err = buildEffectiveTileQuery(
		stored,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS,
		DashboardQueryOverrides{},
		now,
	)
	if err != nil {
		t.Fatalf("buildEffectiveTileQuery stored range: %v", err)
	}
	if !effective.GetTimeRange().GetFrom().AsTime().Equal(storedFrom) {
		t.Fatalf("time range from = %v, want stored %v", effective.GetTimeRange().GetFrom().AsTime(), storedFrom)
	}
	if !effective.GetTimeRange().GetTo().AsTime().Equal(storedTo) {
		t.Fatalf("time range to = %v, want stored %v", effective.GetTimeRange().GetTo().AsTime(), storedTo)
	}
	if effective.GetGranularity() != insightsv1.Granularity_GRANULARITY_WEEK {
		t.Fatalf("granularity = %v, want WEEK", effective.GetGranularity())
	}

	storedWithoutRange := proto.Clone(stored).(*insightsv1.QueryRequest)
	storedWithoutRange.TimeRange = nil
	effective, err = buildEffectiveTileQuery(
		storedWithoutRange,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS,
		DashboardQueryOverrides{},
		now,
	)
	if err != nil {
		t.Fatalf("buildEffectiveTileQuery preset fallback: %v", err)
	}
	if effective.GetTimeRange().GetTo().AsTime() != now {
		t.Fatalf("to = %v, want %v", effective.GetTimeRange().GetTo().AsTime(), now)
	}
}

func TestQueryDashboardTiles_PreservesDashboardTileOrder(t *testing.T) {
	dashboard := DashboardWithTiles{
		Tiles: []dbread.DashboardTile{
			{ID: "markdown", Kind: int16(TileKindMarkdown)},
			{ID: "insight-a", Kind: int16(TileKindInsight)},
			{ID: "insight-b", Kind: int16(TileKindInsight)},
		},
	}

	outcomes := QueryDashboardTiles(context.Background(), nil, dashboard, DashboardQueryOverrides{})
	if len(outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(outcomes))
	}
	if outcomes[0].TileID != "insight-a" || outcomes[1].TileID != "insight-b" {
		t.Fatalf("outcome order = [%q, %q], want [insight-a, insight-b]", outcomes[0].TileID, outcomes[1].TileID)
	}
}
