package events

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	chq "github.com/fivebitsio/cotton/internal/core/clickhouse"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	"github.com/fivebitsio/cotton/internal/geo"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/fivebitsio/cotton/internal/useragent"
)

type Event struct {
	AutoProperties   map[string]string
	CustomProperties map[string]string
	DistinctID       string
	EventID          string
	InsertTime       time.Time
	Kind             string
	OccurTime        time.Time
	ProjectID        string
	SessionID        string
}

// eventColumns is the SELECT column list for the events table.
// Order must match scanEvent.
const eventColumns = `auto_properties, custom_properties, distinct_id, event_id, insert_time, kind, occur_time, project_id, session_id`

func scanEvent(rows driver.Rows) (Event, error) {
	var e Event
	if err := rows.Scan(
		&e.AutoProperties,
		&e.CustomProperties,
		&e.DistinctID,
		&e.EventID,
		&e.InsertTime,
		&e.Kind,
		&e.OccurTime,
		&e.ProjectID,
		&e.SessionID,
	); err != nil {
		return Event{}, err
	}
	return e, nil
}

type Reader struct {
	ch driver.Conn
}

// ErrInvalidFilter indicates a client-caused filter validation error (e.g., unsupported
// operator, invalid numeric value). Handlers should return CodeInvalidArgument, not CodeInternal.
var ErrInvalidFilter = errors.New("invalid filter")

func NewReader(ch driver.Conn) *Reader {
	return &Reader{ch: ch}
}

// getAliasIDs returns all alias IDs for a profile.
func (r *Reader) getAliasIDs(ctx context.Context, projectID, profileID string) ([]string, error) {
	sql, args, err := chq.NewQuery().
		Select("alias_id").
		From("profile_aliases").
		Where(chq.Eq("project_id", projectID), chq.Eq("profile_id", profileID)).
		Build()
	if err != nil {
		return nil, fmt.Errorf("getAliasIDs: build query for project %s profile %s: %w", projectID, profileID, err)
	}

	rows, err := r.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("getAliasIDs: query failed for project %s profile %s: %w", projectID, profileID, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
		}
	}()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("getAliasIDs: scan failed for project %s profile %s: %w", projectID, profileID, err)
		}
		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("getAliasIDs: row iteration failed for project %s profile %s: %w", projectID, profileID, err)
	}
	return ids, nil
}

// resolveProfileIDs returns the primary distinct ID plus any alias IDs for a profile.
// Both projectID and distinctID must be non-empty. At the RPC boundary these are
// guaranteed by MustGetPrincipalWithProject and proto validation (required = true).
func (r *Reader) resolveProfileIDs(ctx context.Context, projectID, distinctID string) ([]string, error) {
	if projectID == "" {
		return nil, fmt.Errorf("resolveProfileIDs: projectID must not be empty")
	}
	if distinctID == "" {
		return nil, fmt.Errorf("resolveProfileIDs: distinctID must not be empty")
	}
	aliasIDs, err := r.getAliasIDs(ctx, projectID, distinctID)
	if err != nil {
		return nil, err
	}
	return append([]string{distinctID}, aliasIDs...), nil
}

const DefaultPageSize int32 = 100
const MaxPageSize int32 = 1000
const DefaultHeatmapDays = 60

// EventCursor is a keyset pagination cursor for event queries (activity feed, event explorer).
// It encodes the occur_time and event_id of the last returned row, used as a
// seek point for the next page. Matches the ORDER BY occur_time DESC, event_id DESC.
type EventCursor struct {
	OccurTime time.Time `json:"t"`
	EventID   string    `json:"e"`
}

// Encode returns the cursor as a base64-encoded JSON string for use as a page token.
// NOTE: Does not validate cursor fields — all call sites construct cursors from
// valid ClickHouse query results. DecodeEventCursor validates on the decode side.
func (c *EventCursor) Encode() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode event cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeEventCursor decodes a base64url-encoded JSON page token.
// Returns an error if the token is malformed or missing required fields (OccurTime, EventID).
func DecodeEventCursor(token string) (*EventCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("invalid page token: %w", err)
	}
	var c EventCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("invalid page token: %w", err)
	}
	if c.OccurTime.IsZero() || c.EventID == "" {
		return nil, fmt.Errorf("invalid page token: missing required cursor fields")
	}
	return &c, nil
}

// EventExplorerParams configures the GetEventExplorer query.
// String filter fields are optional — empty means no filter.
type EventExplorerParams struct {
	ProjectID       string
	DistinctID      string
	SessionID       string
	TimeRange       *commonv1.TimeRange
	PropertyFilters []*commonv1.PropertyFilter
	EventFilters    []*commonv1.EventFilter
	PageSize        int32
	PageToken       *EventCursor
}

func normalizePageSize(pageSize int32) int32 {
	if pageSize <= 0 {
		return DefaultPageSize
	}
	if pageSize > MaxPageSize {
		return MaxPageSize
	}
	return pageSize
}

func applyCommonEventFilters(
	q *chq.Query,
	projectID string,
	timeRange *commonv1.TimeRange,
	propertyFilters []*commonv1.PropertyFilter,
	pageToken *EventCursor,
) error {
	// time_range itself is optional (no required annotation). When present,
	// From/To within it are guaranteed non-nil by proto validation (required fields + validate interceptor).
	// If called outside the RPC chain, callers must ensure From and To are set.
	if timeRange != nil {
		q.Where(
			chq.Gte("occur_time", timeRange.GetFrom().AsTime()),
			chq.Lt("occur_time", timeRange.GetTo().AsTime()),
		)
	}

	for _, f := range propertyFilters {
		cond, err := chq.PropertyCondition(f, projectID)
		if err != nil {
			return err
		}
		q.Where(cond)
	}

	if pageToken != nil {
		q.Where(chq.Or(
			chq.Lt("occur_time", pageToken.OccurTime),
			chq.And(
				chq.Eq("occur_time", pageToken.OccurTime),
				chq.Lt("event_id", pageToken.EventID),
			),
		))
	}

	return nil
}

// GetEventExplorer returns a paginated, filtered list of events across all users in a project.
// It does not resolve aliases. Pagination is cursor-based on (occur_time DESC, event_id DESC).
// PageSize defaults to 100 and is capped at 1000. A nil returned cursor means no more pages.
func (r *Reader) GetEventExplorer(ctx context.Context, params EventExplorerParams) ([]Event, *EventCursor, error) {
	eventCond, err := chq.EventCondition(params.EventFilters, params.ProjectID)
	if err != nil {
		return nil, nil, fmt.Errorf("GetEventExplorer: %w: %w", ErrInvalidFilter, err)
	}

	q := chq.NewQuery().
		Select(eventColumns).
		From("events").
		Where(
			chq.Eq("project_id", params.ProjectID),
			chq.When(params.DistinctID != "", chq.Eq("distinct_id", params.DistinctID)),
			eventCond,
			chq.When(params.SessionID != "", chq.Eq("session_id", params.SessionID)),
		)

	if err := applyCommonEventFilters(q, params.ProjectID, params.TimeRange, params.PropertyFilters, params.PageToken); err != nil {
		return nil, nil, fmt.Errorf("GetEventExplorer: %w: %w", ErrInvalidFilter, err)
	}

	pageSize := normalizePageSize(params.PageSize)

	sql, args, err := q.OrderBy("occur_time DESC", "event_id DESC").Limit(int64(pageSize)).Build()
	if err != nil {
		return nil, nil, fmt.Errorf("GetEventExplorer: build query failed for project %s: %w", params.ProjectID, err)
	}

	rows, err := r.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("GetEventExplorer: query failed for project %s: %w", params.ProjectID, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
		}
	}()

	var events []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("GetEventExplorer: scan failed: %w", err)
		}
		events = append(events, e)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("GetEventExplorer: row iteration failed: %w", err)
	}

	var nextCursor *EventCursor
	if int32(len(events)) == pageSize {
		last := events[len(events)-1]
		nextCursor = &EventCursor{
			OccurTime: last.OccurTime,
			EventID:   last.EventID,
		}
	}

	return events, nextCursor, nil
}

// ActivityFeedParams configures the GetActivityFeed query.
// String filter fields are optional — empty means no filter. TimeRange and PageToken use nil.
type ActivityFeedParams struct {
	ProjectID       string
	DistinctID      string
	SessionID       string
	TimeRange       *commonv1.TimeRange
	PropertyFilters []*commonv1.PropertyFilter
	EventFilters    []*commonv1.EventFilter
	PageSize        int32
	PageToken       *EventCursor
}

// GetActivityFeed returns a paginated, filtered list of events for a profile.
// It resolves alias IDs (merged anonymous profiles). Background merges provide
// sufficient deduplication; FINAL is not needed. Pagination is cursor-based on (occur_time DESC, event_id DESC).
// PageSize defaults to 100 and is capped at 1000. A nil returned cursor means no more pages.
//
// ProjectID and DistinctID are required. At the RPC boundary these are guaranteed by
// MustGetPrincipalWithProject (non-empty project ID) and proto validation (required = true).
// Internal callers must ensure both are non-empty — empty values return an error.
func (r *Reader) GetActivityFeed(ctx context.Context, params ActivityFeedParams) ([]Event, *EventCursor, error) {
	ids, err := r.resolveProfileIDs(ctx, params.ProjectID, params.DistinctID)
	if err != nil {
		return nil, nil, fmt.Errorf("GetActivityFeed: %w", err)
	}

	eventCond, err := chq.EventCondition(params.EventFilters, params.ProjectID)
	if err != nil {
		return nil, nil, fmt.Errorf("GetActivityFeed: %w: %w", ErrInvalidFilter, err)
	}

	q := chq.NewQuery().
		Select(eventColumns).
		From("events").
		Where(
			chq.Eq("project_id", params.ProjectID),
			chq.RawCond("distinct_id IN ?", ids),
			eventCond,
			chq.When(params.SessionID != "", chq.Eq("session_id", params.SessionID)),
		)

	if err := applyCommonEventFilters(q, params.ProjectID, params.TimeRange, params.PropertyFilters, params.PageToken); err != nil {
		return nil, nil, fmt.Errorf("GetActivityFeed: %w: %w", ErrInvalidFilter, err)
	}

	pageSize := normalizePageSize(params.PageSize)

	sql, args, err := q.OrderBy("occur_time DESC", "event_id DESC").Limit(int64(pageSize)).Build()
	if err != nil {
		return nil, nil, fmt.Errorf("GetActivityFeed: build query failed for project %s: %w", params.ProjectID, err)
	}

	rows, err := r.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("GetActivityFeed: query failed for project %s: %w", params.ProjectID, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
		}
	}()

	var events []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("GetActivityFeed: scan failed: %w", err)
		}
		events = append(events, e)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("GetActivityFeed: row iteration failed: %w", err)
	}

	var nextCursor *EventCursor
	if int32(len(events)) == pageSize {
		last := events[len(events)-1]
		nextCursor = &EventCursor{
			OccurTime: last.OccurTime,
			EventID:   last.EventID,
		}
	}

	return events, nextCursor, nil
}

// HeatmapDay holds the event count for a single calendar day.
type HeatmapDay struct {
	Date  string // YYYY-MM-DD (UTC)
	Count int64
}

// scanHeatmapDays scans per-day event counts from a ClickHouse result set.
// Expects columns: (day string, cnt uint64).
func scanHeatmapDays(rows driver.Rows) ([]HeatmapDay, error) {
	var days []HeatmapDay
	for rows.Next() {
		var day string
		var cnt uint64
		if err := rows.Scan(&day, &cnt); err != nil {
			return nil, fmt.Errorf("scan heatmap day: %w", err)
		}
		days = append(days, HeatmapDay{Date: day, Count: int64(cnt)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("heatmap row iteration: %w", err)
	}
	return days, nil
}

// queryHeatmap runs a per-day event count query for the given profile IDs and optional time range.
// The time range is half-open: [from, to). Zero-value times omit that bound.
func (r *Reader) queryHeatmap(ctx context.Context, projectID string, ids []string, from, to time.Time) ([]HeatmapDay, error) {
	q := chq.NewQuery().
		Select("toString(toDate(occur_time)) AS day", "count() AS cnt").
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.RawCond("distinct_id IN ?", ids),
			chq.When(!from.IsZero(), chq.Gte("occur_time", from)),
			chq.When(!to.IsZero(), chq.Lt("occur_time", to)),
		)

	sql, args, err := q.GroupBy("day").OrderBy("day").Build()
	if err != nil {
		return nil, fmt.Errorf("queryHeatmap: build query failed for project %s: %w", projectID, err)
	}

	rows, err := r.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("queryHeatmap: query failed for project %s: %w", projectID, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
		}
	}()

	return scanHeatmapDays(rows)
}

// ActivityHeatmapParams configures the GetActivityHeatmap query.
type ActivityHeatmapParams struct {
	ProjectID  string
	DistinctID string
	TimeRange  *commonv1.TimeRange
}

// GetActivityHeatmap returns per-day event counts for a profile over the given window.
// The time range is half-open: [from, to). When TimeRange is nil, no time filter is applied;
// callers are responsible for providing a range (the RPC handler defaults to 60 days).
// Alias IDs are resolved so merged anonymous events are included.
//
// ProjectID and DistinctID are required. At the RPC boundary these are guaranteed by
// MustGetPrincipalWithProject (non-empty project ID) and proto validation (required = true).
// Internal callers must ensure both are non-empty — empty values return an error.
func (r *Reader) GetActivityHeatmap(ctx context.Context, params ActivityHeatmapParams) ([]HeatmapDay, error) {
	ids, err := r.resolveProfileIDs(ctx, params.ProjectID, params.DistinctID)
	if err != nil {
		return nil, fmt.Errorf("GetActivityHeatmap: %w", err)
	}

	var from, to time.Time
	if params.TimeRange != nil {
		from = params.TimeRange.GetFrom().AsTime()
		to = params.TimeRange.GetTo().AsTime()
		if from.IsZero() || to.IsZero() {
			return nil, fmt.Errorf("GetActivityHeatmap: TimeRange.From and TimeRange.To must be non-zero when TimeRange is set")
		}
	}

	days, err := r.queryHeatmap(ctx, params.ProjectID, ids, from, to)
	if err != nil {
		return nil, fmt.Errorf("GetActivityHeatmap: %w", err)
	}
	return days, nil
}

// ProfileStats holds aggregate event statistics and device/location context
// derived from the profile's latest event.
type ProfileStats struct {
	FirstSeen      time.Time
	LastSeen       time.Time
	TotalEvents    int64
	Browser        string
	BrowserVersion string
	OS             string
	OSVersion      string
	Device         string
	Country        string
	City           string
	IP             string
}

// queryProfileStats runs the aggregate stats query and extracts auto_properties
// from the latest event. Returns nil when no events exist for the given IDs
// (determined by total_events == 0; ClickHouse aggregates always produce a row).
func (r *Reader) queryProfileStats(ctx context.Context, projectID string, ids []string) (*ProfileStats, error) {
	statSQL, statArgs, err := chq.NewQuery().
		Select(
			"min(occur_time) AS first_seen",
			"max(occur_time) AS last_seen",
			"count() AS total_events",
			"argMax(auto_properties, occur_time) AS latest_props",
		).
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.RawCond("distinct_id IN ?", ids),
		).
		Build()
	if err != nil {
		return nil, fmt.Errorf("queryProfileStats: build query failed for project %s: %w", projectID, err)
	}

	rows, err := r.ch.Query(ctx, statSQL, statArgs...)
	if err != nil {
		return nil, fmt.Errorf("queryProfileStats: query failed for project %s: %w", projectID, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
		}
	}()

	// ClickHouse aggregates without GROUP BY always return one row. Zero rows
	// indicates a driver bug or connection issue — return an error.
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("queryProfileStats: row iteration failed: %w", err)
		}
		return nil, fmt.Errorf("queryProfileStats: aggregate query returned no rows (unexpected)")
	}

	var stats ProfileStats
	var totalEvents uint64
	var latestProps map[string]string
	if err := rows.Scan(&stats.FirstSeen, &stats.LastSeen, &totalEvents, &latestProps); err != nil {
		return nil, fmt.Errorf("queryProfileStats: scan failed: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queryProfileStats: row iteration failed: %w", err)
	}

	stats.TotalEvents = int64(totalEvents)
	if stats.TotalEvents == 0 {
		return nil, nil
	}

	stats.Browser = latestProps[useragent.PropBrowser]
	stats.BrowserVersion = latestProps[useragent.PropBrowserVersion]
	stats.OS = latestProps[useragent.PropOS]
	stats.OSVersion = latestProps[useragent.PropOSVersion]
	stats.Device = latestProps[useragent.PropDevice]
	stats.Country = latestProps[geo.PropCountry]
	stats.City = latestProps[geo.PropCity]
	stats.IP = latestProps[geo.PropIP]

	return &stats, nil
}

// GetProfileStats returns aggregate event statistics and latest-event context (over all time),
// plus a per-day activity heatmap for the last DefaultHeatmapDays, for a profile.
// Alias IDs are resolved so merged anonymous events are included.
// Returns nil, nil, nil if the profile has no recorded events (the heatmap query is skipped).
//
// ProjectID and DistinctID are required. At the RPC boundary these are guaranteed by
// MustGetPrincipalWithProject (non-empty project ID) and proto validation (required = true).
// Internal callers must ensure both are non-empty — empty values return an error.
func (r *Reader) GetProfileStats(ctx context.Context, projectID, distinctID string) (*ProfileStats, []HeatmapDay, error) {
	ids, err := r.resolveProfileIDs(ctx, projectID, distinctID)
	if err != nil {
		return nil, nil, fmt.Errorf("GetProfileStats: %w", err)
	}

	stats, err := r.queryProfileStats(ctx, projectID, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("GetProfileStats: %w", err)
	}
	if stats == nil {
		return nil, nil, nil
	}

	now := time.Now().UTC()
	days, err := r.queryHeatmap(ctx, projectID, ids, now.AddDate(0, 0, -DefaultHeatmapDays), now)
	if err != nil {
		return nil, nil, fmt.Errorf("GetProfileStats: %w", err)
	}

	return stats, days, nil
}
