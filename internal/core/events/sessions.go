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

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"github.com/pug-sh/pug/internal/slogx"
)

// ProfileSessionSort orders a profile's session list. Always descending, with the
// session id breaking ties.
type ProfileSessionSort string

const (
	ProfileSessionSortStartedAt  ProfileSessionSort = "started_at"
	ProfileSessionSortDuration   ProfileSessionSort = "duration"
	ProfileSessionSortEventCount ProfileSessionSort = "event_count"
)

// sessionSpillThresholdBytes mirrors insightsSpillThresholdBytes: this query holds
// one aggregation state per session over a profile's whole history, so a heavy
// profile should spill to disk rather than hit the ClickHouse memory limit.
const sessionSpillThresholdBytes = 1 << 30

// ErrPageTokenSortMismatch means a page token was replayed under a different sort.
// The seek predicate is built from the ordering column, so honouring it would skip
// or repeat sessions.
var ErrPageTokenSortMismatch = errors.New("page token was issued for a different sort")

// ProfileSession is one session of a profile, aggregated over every event in it.
type ProfileSession struct {
	SessionID  string
	StartedAt  time.Time
	EndedAt    time.Time
	EventCount int64
	Browser    string
	OS         string
	Device     string
	Platform   string
	Bot        bool
}

// ProfileSessionCursor is a keyset cursor over a profile's session list. Value is
// the ordering column for Sort: epoch millis, duration millis, or event count.
type ProfileSessionCursor struct {
	Sort      ProfileSessionSort `json:"s"`
	Value     int64              `json:"v"`
	SessionID string             `json:"i"`
}

func (c *ProfileSessionCursor) Encode() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode profile session cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeProfileSessionCursor decodes a base64url-encoded JSON page token.
func DecodeProfileSessionCursor(token string) (*ProfileSessionCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("invalid page token: %w", err)
	}
	// Value is a pointer so an absent "v" stays distinguishable from a legitimate 0:
	// a truncated token that zero-fills the seek would read as end-of-list.
	var wire struct {
		Sort      ProfileSessionSort `json:"s"`
		Value     *int64             `json:"v"`
		SessionID string             `json:"i"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return nil, fmt.Errorf("invalid page token: %w", err)
	}
	if wire.SessionID == "" || wire.Value == nil {
		return nil, errors.New("invalid page token: missing required cursor fields")
	}
	c := ProfileSessionCursor{Sort: wire.Sort, Value: *wire.Value, SessionID: wire.SessionID}
	switch c.Sort {
	case ProfileSessionSortStartedAt:
		// Any int64: a device clock at or before the epoch yields non-positive millis.
	case ProfileSessionSortEventCount:
		if c.Value <= 0 {
			return nil, fmt.Errorf("invalid page token: non-positive event_count value %d", c.Value)
		}
	case ProfileSessionSortDuration:
		// 0 is legitimate here — a single-event session.
		if c.Value < 0 {
			return nil, fmt.Errorf("invalid page token: negative duration value %d", c.Value)
		}
	default:
		return nil, fmt.Errorf("invalid page token: unknown sort %q", c.Sort)
	}
	return &c, nil
}

// ProfileSessionsParams configures the GetProfileSessions query.
// TimeRange is optional; nil means the profile's whole history.
type ProfileSessionsParams struct {
	ProjectID   string
	DistinctID  string
	PageSize    int32
	PageToken   *ProfileSessionCursor
	Sort        ProfileSessionSort
	TimeRange   *commonv1.TimeRange
	IncludeBots bool
}

// profileSessionColumns is the SELECT list; order must match scanProfileSession,
// and profileSessionOrderExpr references these aliases.
//
// all_bot must not alias `bot` — HAVING max(bot) would bind the alias and fail as
// max(min(bot)); `session` must stay a String alias because the cursor compares as
// a string and ClickHouse orders UUIDs differently from their text.
//
// Promoted columns only: reading a property out of auto_properties would pull the
// table's widest column through every scanned granule for a device label.
var profileSessionColumns = []string{
	"toString(session_id) AS session",
	"min(occur_time) AS started_at",
	"max(occur_time) AS ended_at",
	"toInt64(count()) AS event_count",
	"argMax(browser, occur_time) AS latest_browser",
	"argMax(os, occur_time) AS latest_os",
	"argMax(device, occur_time) AS latest_device",
	"argMax(platform, occur_time) AS latest_platform",
	"toUInt8(min(bot)) AS all_bot",
}

// profileSessionOrderExpr is the ordering column, as an Int64 so one keyset
// predicate serves every sort.
func profileSessionOrderExpr(sort ProfileSessionSort) string {
	switch sort {
	case ProfileSessionSortDuration:
		return "toUnixTimestamp64Milli(ended_at) - toUnixTimestamp64Milli(started_at)"
	case ProfileSessionSortEventCount:
		return "event_count"
	case ProfileSessionSortStartedAt:
		return "toUnixTimestamp64Milli(started_at)"
	default:
		return "toUnixTimestamp64Milli(started_at)"
	}
}

// profileSessionSortValue is the Go side of profileSessionOrderExpr; the two must
// agree or a page seeks on a value the previous page never ordered by. They agree
// only because occur_time is DateTime64(3) — at finer precision SQL's
// difference-of-truncations and Go's truncation-of-difference drift by up to 1ms.
func profileSessionSortValue(sort ProfileSessionSort, s ProfileSession) int64 {
	switch sort {
	case ProfileSessionSortDuration:
		return s.EndedAt.Sub(s.StartedAt).Milliseconds()
	case ProfileSessionSortEventCount:
		return s.EventCount
	case ProfileSessionSortStartedAt:
		return s.StartedAt.UnixMilli()
	default:
		return s.StartedAt.UnixMilli()
	}
}

func normalizeProfileSessionSort(sort ProfileSessionSort) ProfileSessionSort {
	switch sort {
	case ProfileSessionSortDuration, ProfileSessionSortEventCount, ProfileSessionSortStartedAt:
		return sort
	default:
		return ProfileSessionSortStartedAt
	}
}

func scanProfileSession(rows driver.Rows) (ProfileSession, error) {
	var s ProfileSession
	var bot uint8
	if err := rows.Scan(
		&s.SessionID,
		&s.StartedAt,
		&s.EndedAt,
		&s.EventCount,
		&s.Browser,
		&s.OS,
		&s.Device,
		&s.Platform,
		&bot,
	); err != nil {
		return ProfileSession{}, err
	}
	s.Bot = bot != 0
	return s, nil
}

// GetProfileSessions returns a paginated list of a profile's sessions, one row per
// session_id aggregated over every event in it. Alias IDs are resolved so merged
// anonymous events are included. PageSize defaults to 100, capped at 1000; a nil
// returned cursor means no more pages.
//
// Every page aggregates the profile's history over TimeRange — the keyset seek
// filters sessions, not the events they are built from — so start, duration and
// count are exact however many events the profile has.
//
// Bot exclusion is session-level: a session with any tagged event is dropped whole
// rather than returned with a short count. That diverges from the profile's
// row-level session count (see docs/architecture/profiles.md).
func (r *Reader) GetProfileSessions(
	ctx context.Context, params ProfileSessionsParams,
) ([]ProfileSession, *ProfileSessionCursor, error) {
	sort := normalizeProfileSessionSort(params.Sort)
	if params.PageToken != nil && params.PageToken.Sort != sort {
		return nil, nil, fmt.Errorf("GetProfileSessions: %w: token sort %q, request sort %q",
			ErrPageTokenSortMismatch, params.PageToken.Sort, sort)
	}

	ids, err := r.resolveProfileIDs(ctx, params.ProjectID, params.DistinctID)
	if err != nil {
		return nil, nil, fmt.Errorf("GetProfileSessions: %w", err)
	}

	orderExpr := profileSessionOrderExpr(sort)
	pageSize := normalizePageSize(params.PageSize)

	q := chq.NewQuery().
		Select(profileSessionColumns...).
		From("events").
		Where(
			chq.Eq("project_id", params.ProjectID),
			chq.RawCond("distinct_id IN ?", ids),
		).
		GroupBy("session_id")

	if params.TimeRange != nil {
		if params.TimeRange.GetFrom() == nil || params.TimeRange.GetTo() == nil {
			err := errors.New("GetProfileSessions: TimeRange.From and TimeRange.To must be set when TimeRange is provided")
			slog.ErrorContext(ctx, "GetProfileSessions called with partial TimeRange", slogx.Error(err),
				slog.String("project_id", params.ProjectID))
			telemetry.RecordError(ctx, err)
			return nil, nil, err
		}
		q.Where(
			chq.Gte("occur_time", params.TimeRange.GetFrom().AsTime()),
			chq.Lt("occur_time", params.TimeRange.GetTo().AsTime()),
		)
	}

	chq.SessionBotHaving(q, params.IncludeBots)
	if params.PageToken != nil {
		q.HavingExpr(fmt.Sprintf("(%s, session) < (?, ?)", orderExpr),
			params.PageToken.Value, params.PageToken.SessionID)
	}

	// One row past the page so a cursor is emitted only when a further row exists —
	// an exactly-full final page must not hand back a token to an empty page.
	sql, args, err := q.
		OrderBy(orderExpr+" DESC", "session DESC").
		Limit(int64(pageSize) + 1).
		WithSpillThreshold(sessionSpillThresholdBytes).
		Build()
	if err != nil {
		slog.ErrorContext(ctx, "GetProfileSessions: build query failed", slogx.Error(err),
			slog.String("project_id", params.ProjectID))
		telemetry.RecordError(ctx, err)
		return nil, nil, fmt.Errorf("GetProfileSessions: build query failed for project %s: %w", params.ProjectID, err)
	}

	rows, err := r.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, recordSessionQueryErr(ctx, params.ProjectID, "clickhouse query failed", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()

	var sessions []ProfileSession
	for rows.Next() {
		s, err := scanProfileSession(rows)
		if err != nil {
			return nil, nil, recordSessionQueryErr(ctx, params.ProjectID, "scan failed", err)
		}
		sessions = append(sessions, s)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, recordSessionQueryErr(ctx, params.ProjectID, "row iteration failed", err)
	}

	var nextCursor *ProfileSessionCursor
	if int32(len(sessions)) > pageSize {
		sessions = sessions[:pageSize]
		last := sessions[len(sessions)-1]
		nextCursor = &ProfileSessionCursor{
			Sort:      sort,
			Value:     profileSessionSortValue(sort, last),
			SessionID: last.SessionID,
		}
	}

	return sessions, nextCursor, nil
}

// recordSessionQueryErr logs and records at the detecting layer, except for a
// cancelled or timed-out context: this query is long enough that a client
// navigating away is routine, and the handler already maps it to the right code.
func recordSessionQueryErr(ctx context.Context, projectID, stage string, err error) error {
	wrapped := fmt.Errorf("GetProfileSessions: %s for project %s: %w", stage, projectID, err)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return wrapped
	}
	slog.ErrorContext(ctx, "GetProfileSessions: "+stage, slogx.Error(err),
		slog.String("project_id", projectID))
	telemetry.RecordError(ctx, err)
	return wrapped
}
