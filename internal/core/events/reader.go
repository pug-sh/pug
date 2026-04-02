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
	"github.com/fivebitsio/cotton/internal/slogx"
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

const DefaultPageSize int32 = 100
const MaxPageSize int32 = 1000

// ActivityFeedCursor is a keyset pagination cursor for the activity feed.
// It encodes the occur_time and event_id of the last returned row, used as a
// seek point for the next page. Matches the ORDER BY occur_time DESC, event_id DESC.
type ActivityFeedCursor struct {
	OccurTime time.Time `json:"t"`
	EventID   string    `json:"e"`
}

// Encode returns the cursor as a base64-encoded JSON string for use as a page token.
// NOTE: Does not validate cursor fields — the only call site constructs cursors from
// valid ClickHouse query results. DecodeActivityFeedCursor validates on the decode side.
func (c *ActivityFeedCursor) Encode() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode activity feed cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeActivityFeedCursor decodes a base64url-encoded JSON page token.
// Returns an error if the token is malformed or missing required fields (OccurTime, EventID).
func DecodeActivityFeedCursor(token string) (*ActivityFeedCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("invalid page token: %w", err)
	}
	var c ActivityFeedCursor
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
	PageToken       *ActivityFeedCursor
}

// GetEventExplorer returns a paginated, filtered list of events across all users in a project.
// It does not resolve aliases. Pagination is cursor-based on (occur_time DESC, event_id DESC).
// PageSize defaults to 100 and is capped at 1000. A nil returned cursor means no more pages.
func (r *Reader) GetEventExplorer(ctx context.Context, params EventExplorerParams) ([]Event, *ActivityFeedCursor, error) {
	eventCond, err := chq.EventCondition(params.EventFilters)
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

	// NOTE: From/To are guaranteed non-nil by proto validation (required fields + validate interceptor).
	// If called outside the RPC chain, callers must ensure From and To are set.
	if params.TimeRange != nil {
		q.Where(
			chq.Gte("occur_time", params.TimeRange.GetFrom().AsTime()),
			chq.Lt("occur_time", params.TimeRange.GetTo().AsTime()),
		)
	}

	for _, f := range params.PropertyFilters {
		cond, err := chq.PropertyCondition(f)
		if err != nil {
			return nil, nil, fmt.Errorf("GetEventExplorer: %w: %w", ErrInvalidFilter, err)
		}
		q.Where(cond)
	}

	if params.PageToken != nil {
		q.Where(chq.Or(
			chq.Lt("occur_time", params.PageToken.OccurTime),
			chq.And(
				chq.Eq("occur_time", params.PageToken.OccurTime),
				chq.Lt("event_id", params.PageToken.EventID),
			),
		))
	}

	pageSize := params.PageSize
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}

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

	var nextCursor *ActivityFeedCursor
	if int32(len(events)) == pageSize {
		last := events[len(events)-1]
		nextCursor = &ActivityFeedCursor{
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
	PageToken       *ActivityFeedCursor
}

// GetActivityFeed returns a paginated, filtered list of events for a profile.
// It resolves alias IDs (merged anonymous profiles). Background merges provide
// sufficient deduplication; FINAL is not needed. Pagination is cursor-based on (occur_time DESC, event_id DESC).
// PageSize defaults to 100 and is capped at 1000. A nil returned cursor means no more pages.
//
// ProjectID and DistinctID are required. At the RPC boundary these are guaranteed by
// MustGetPrincipalWithProject (non-empty project ID) and proto validation (min_len=1).
// Internal callers must ensure both are non-empty — empty values silently return zero results.
func (r *Reader) GetActivityFeed(ctx context.Context, params ActivityFeedParams) ([]Event, *ActivityFeedCursor, error) {
	aliasIDs, err := r.getAliasIDs(ctx, params.ProjectID, params.DistinctID)
	if err != nil {
		return nil, nil, fmt.Errorf("GetActivityFeed: getAliasIDs failed for project %s: %w", params.ProjectID, err)
	}

	ids := append([]string{params.DistinctID}, aliasIDs...)

	eventCond, err := chq.EventCondition(params.EventFilters)
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

	// NOTE: From/To are guaranteed non-nil by proto validation (required fields + validate interceptor).
	// If called outside the RPC chain, callers must ensure From and To are set — nil values
	// silently resolve to epoch time (1970-01-01) via protobuf's AsTime() nil-receiver behavior.
	if params.TimeRange != nil {
		q.Where(
			chq.Gte("occur_time", params.TimeRange.GetFrom().AsTime()),
			chq.Lt("occur_time", params.TimeRange.GetTo().AsTime()),
		)
	}

	for _, f := range params.PropertyFilters {
		cond, err := chq.PropertyCondition(f)
		if err != nil {
			return nil, nil, fmt.Errorf("GetActivityFeed: %w: %w", ErrInvalidFilter, err)
		}
		q.Where(cond)
	}

	if params.PageToken != nil {
		q.Where(chq.Or(
			chq.Lt("occur_time", params.PageToken.OccurTime),
			chq.And(
				chq.Eq("occur_time", params.PageToken.OccurTime),
				chq.Lt("event_id", params.PageToken.EventID),
			),
		))
	}

	pageSize := params.PageSize
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}

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

	var nextCursor *ActivityFeedCursor
	if int32(len(events)) == pageSize {
		last := events[len(events)-1]
		nextCursor = &ActivityFeedCursor{
			OccurTime: last.OccurTime,
			EventID:   last.EventID,
		}
	}

	return events, nextCursor, nil
}
