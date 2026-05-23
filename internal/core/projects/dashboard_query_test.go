package projects

import (
	"testing"
	"time"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
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
	stored := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Granularity: insightsv1.Granularity_GRANULARITY_WEEK.Enum(),
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(now.Add(-48 * time.Hour)),
			To:   timestamppb.New(now.Add(-24 * time.Hour)),
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
		t.Fatalf("buildEffectiveTileQuery preset: %v", err)
	}
	if effective.GetGranularity() != insightsv1.Granularity_GRANULARITY_WEEK {
		t.Fatalf("granularity = %v, want WEEK", effective.GetGranularity())
	}
	if effective.GetTimeRange().GetTo().AsTime() != now {
		t.Fatalf("to = %v, want %v", effective.GetTimeRange().GetTo().AsTime(), now)
	}
}
