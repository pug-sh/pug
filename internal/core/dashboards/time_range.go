package dashboards

import (
	"context"
	"time"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// startOfDay truncates t to midnight UTC. UTC (not t's local zone) is deliberate:
// the day-keyed ClickHouse rollup is UTC-aligned, and insights.rollupWindowAligned
// only treats a window as rollup-eligible when `from` is exactly midnight UTC. A
// midnight-local boundary on a non-UTC server would silently disqualify every
// default-window dashboard tile and force a raw scan.
func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func lastNHours(now time.Time, n int) *commonv1.TimeRange {
	from := now.Add(-time.Duration(n) * time.Hour)
	return &commonv1.TimeRange{
		From: timestamppb.New(from),
		To:   timestamppb.New(now),
	}
}

// lastNDays spans the N calendar days ending today: from the start of the day
// (N-1) days ago, to now. The span is therefore (N-1) full days plus the partial
// current day — strictly less than N*24h — so the largest preset in each tier
// (e.g. LAST_365_DAYS with GRANULARITY_DAY, LAST_14_DAYS with GRANULARITY_HOUR)
// fits its per-granularity range cap (proto QueryRequest CEL) instead of
// overshooting by the partial day and failing per-tile validation. It also
// yields exactly N daily buckets rather than N+1.
func lastNDays(now time.Time, n int) *commonv1.TimeRange {
	from := startOfDay(now.AddDate(0, 0, -(n - 1)))
	return &commonv1.TimeRange{
		From: timestamppb.New(from),
		To:   timestamppb.New(now),
	}
}

// DashboardDefaultTimeRangePresetFromDB maps the stored dashboards.default_time_range
// enum name to a preset, normalizing unknown/UNSPECIFIED to LAST_30_DAYS.
// Unknown non-empty / non-UNSPECIFIED names (proto rename or DB corruption) are
// logged once per process via LogUnknownEnumOnce so the silent fallback doesn't
// mask a deploy-time bug.
func DashboardDefaultTimeRangePresetFromDB(ctx context.Context, raw string) commonv1.TimeRangePreset {
	value, ok := commonv1.TimeRangePreset_value[raw]
	if ok && value != int32(commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED) {
		return commonv1.TimeRangePreset(value)
	}
	if !ok && raw != "" {
		LogUnknownEnumOnce(ctx, "TimeRangePreset", "dashboards.default_time_range", raw)
	}
	return commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS
}

// DashboardGranularityFromDB maps the stored dashboards.default_granularity enum
// name to a Granularity, normalizing unknown/UNSPECIFIED to DAY. Unknown
// non-empty / non-UNSPECIFIED names are logged once per process.
func DashboardGranularityFromDB(ctx context.Context, raw string) insightsv1.Granularity {
	value, ok := insightsv1.Granularity_value[raw]
	if ok && value != int32(insightsv1.Granularity_GRANULARITY_UNSPECIFIED) {
		return insightsv1.Granularity(value)
	}
	if !ok && raw != "" {
		LogUnknownEnumOnce(ctx, "Granularity", "dashboards.default_granularity", raw)
	}
	return insightsv1.Granularity_GRANULARITY_DAY
}

func validAbsoluteTimeRange(tr *commonv1.TimeRange) bool {
	if tr == nil || tr.GetFrom() == nil || tr.GetTo() == nil {
		return false
	}
	return tr.GetFrom().AsTime().Before(tr.GetTo().AsTime())
}

// ResolveDashboardTimeRangePreset resolves a dashboard tile preset to an absolute
// time range. When the preset is unknown, fallback is used when valid; otherwise
// LAST_30_DAYS is used.
func ResolveDashboardTimeRangePreset(
	preset commonv1.TimeRangePreset,
	fallback *commonv1.TimeRange,
	now time.Time,
) *commonv1.TimeRange {
	switch preset {
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_1_HOUR:
		return lastNHours(now, 1)
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_6_HOURS:
		return lastNHours(now, 6)
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_24_HOURS:
		return lastNHours(now, 24)
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS:
		return lastNDays(now, 7)
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS:
		return lastNDays(now, 14)
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS:
		return lastNDays(now, 30)
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS:
		return lastNDays(now, 90)
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS:
		return lastNDays(now, 180)
	case commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_365_DAYS:
		return lastNDays(now, 365)
	default:
		if validAbsoluteTimeRange(fallback) {
			return fallback
		}
		return lastNDays(now, 30)
	}
}
