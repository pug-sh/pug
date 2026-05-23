package projects

import (
	"time"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func lastNHours(now time.Time, n int) *commonv1.TimeRange {
	from := now.Add(-time.Duration(n) * time.Hour)
	return &commonv1.TimeRange{
		From: timestamppb.New(from),
		To:   timestamppb.New(now),
	}
}

func lastNDays(now time.Time, n int) *commonv1.TimeRange {
	from := startOfDay(now.AddDate(0, 0, -n))
	return &commonv1.TimeRange{
		From: timestamppb.New(from),
		To:   timestamppb.New(now),
	}
}

// TileDefaultTimeRangePresetFromDB normalizes the stored default_time_range column
// for the given tile kind.
func TileDefaultTimeRangePresetFromDB(kind TileKind, raw string) commonv1.TimeRangePreset {
	if kind != TileKindInsight {
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED
	}
	preset := commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS
	if value, ok := commonv1.TimeRangePreset_value[raw]; ok {
		preset = commonv1.TimeRangePreset(value)
	}
	tr := normalizedTileDefaultTimeRange(TileKindInsight, preset)
	name := tileDefaultTimeRangeDBName(tr)
	value, ok := commonv1.TimeRangePreset_value[name]
	if !ok {
		return commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS
	}
	return commonv1.TimeRangePreset(value)
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
		if fallback != nil && fallback.GetFrom() != nil && fallback.GetTo() != nil {
			if fallback.GetFrom().AsTime().Before(fallback.GetTo().AsTime()) {
				return fallback
			}
		}
		return lastNDays(now, 30)
	}
}
