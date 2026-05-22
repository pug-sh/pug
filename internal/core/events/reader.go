package events

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"github.com/pug-sh/pug/internal/geo"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/pug-sh/pug/internal/useragent"
)

var unrecognisedVariantSlotCounter metric.Int64Counter

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/core/events")
	unrecognisedVariantSlotCounter, _ = meter.Int64Counter(
		"events.unrecognized_variant_slot_total",
		metric.WithDescription("Variant slots returned by ClickHouse whose Go type the unwrap switch doesn't recognise. Coerced to string. Indicates schema drift."),
	)
}

type Event struct {
	// AutoProperties and CustomProperties both hold event properties decoded
	// from Map(String, Variant(...)). Values are native Go types matching the
	// active variant: string, int64, float64, bool. Timestamps are normalised
	// to RFC3339Nano UTC strings by unwrapPropertyMap, so callers never see
	// time.Time. chcol.Variant exposure is contained to the scan boundary.
	AutoProperties   map[string]any
	CustomProperties map[string]any
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

func scanEvent(ctx context.Context, rows driver.Rows) (Event, error) {
	var e Event
	var rawAuto map[string]chcol.Variant
	var rawCustom map[string]chcol.Variant
	if err := rows.Scan(
		&rawAuto,
		&rawCustom,
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
	e.AutoProperties = unwrapPropertyMap(ctx, rawAuto)
	e.CustomProperties = unwrapCustomProperties(ctx, rawCustom)
	return e, nil
}

// unwrapPropertyMap unwraps the driver's map[string]chcol.Variant scan
// type into native Go values. Timestamps are normalised to RFC3339Nano UTC
// strings so JSON marshalers and structpb consumers don't need a time.Time
// special case. Currently-known Variant slot types (string, int64, float64,
// bool, time.Time) pass through as native Go values; any future slot type the
// switch doesn't recognise is coerced to its fmt-default string so downstream
// structpb consumers don't 500, and the drift is logged at WARN.
func unwrapPropertyMap(ctx context.Context, raw map[string]chcol.Variant) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		switch x := v.Any().(type) {
		case nil:
			out[k] = nil
		case string, int64, float64, bool:
			out[k] = x
		case time.Time:
			out[k] = x.UTC().Format(time.RFC3339Nano)
		default:
			goType := fmt.Sprintf("%T", x)
			err := fmt.Errorf("unrecognised Variant slot type %s for key %q", goType, k)
			slog.WarnContext(ctx, "unwrapPropertyMap: coercing unrecognised Variant slot to string",
				slogx.Error(err), slog.String("property_key", k), slog.String("go_type", goType))
			telemetry.RecordError(ctx, err)
			unrecognisedVariantSlotCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("go_type", goType),
			))
			// Prefix the coerced value with a sentinel so dashboard users
			// see something obviously broken rather than a malformed-looking
			// real value (e.g. byte-slice "[10 11 12]"). Pairs with the WARN
			// log + counter for operator visibility.
			out[k] = fmt.Sprintf("<unrecognized variant: %s> %v", goType, x)
		}
	}
	return out
}

func unwrapCustomProperties(ctx context.Context, raw map[string]chcol.Variant) map[string]any {
	return unwrapPropertyMap(ctx, raw)
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

// getAliasIDs returns the alias IDs that currently map to a profile. The
// profile_aliases table records each alias write as a row keyed by
// (project_id, profile_id, alias_id). Reassigning an alias to a new profile
// produces a row under a different sort-key tuple, so the old and new
// mappings coexist after merge. Latest-row semantics (argMax(profile_id,
// insert_time) per alias_id) surface only the current mapping.
func (r *Reader) getAliasIDs(ctx context.Context, projectID, profileID string) ([]string, error) {
	sql, args, err := chq.NewQuery().
		With("latest_profile_aliases", latestProfileAliasesQuery(projectID)).
		Select("alias_id").
		From("latest_profile_aliases").
		Where(chq.Eq("profile_id", profileID)).
		Build()
	if err != nil {
		slog.ErrorContext(ctx, "getAliasIDs: build query failed", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("profile_id", profileID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("getAliasIDs: build query for project %s profile %s: %w", projectID, profileID, err)
	}

	rows, err := r.ch.Query(ctx, sql, args...)
	if err != nil {
		slog.ErrorContext(ctx, "getAliasIDs: clickhouse query failed", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("profile_id", profileID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("getAliasIDs: query failed for project %s profile %s: %w", projectID, profileID, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			slog.ErrorContext(ctx, "getAliasIDs: scan failed", slogx.Error(err),
				slog.String("project_id", projectID), slog.String("profile_id", profileID))
			telemetry.RecordError(ctx, err)
			return nil, fmt.Errorf("getAliasIDs: scan failed for project %s profile %s: %w", projectID, profileID, err)
		}
		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "getAliasIDs: row iteration failed", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("profile_id", profileID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("getAliasIDs: row iteration failed for project %s profile %s: %w", projectID, profileID, err)
	}
	return ids, nil
}

// getSingleString runs a query that may return zero or one string result.
// logFieldKey is the slog/error label for the identifier (e.g. "distinct_id"
// or "profile_id") so logs and error wraps describe the correct column when
// the same helper is reused across resolution stages. Returns (value, true,
// nil) on a single row, ("", false, nil) on zero rows, and an error on driver
// failure.
func (r *Reader) getSingleString(
	ctx context.Context,
	logPrefix string,
	logFieldKey string,
	sql string,
	args []any,
	projectID string,
	identifier string,
) (string, bool, error) {
	rows, err := r.ch.Query(ctx, sql, args...)
	if err != nil {
		slog.ErrorContext(ctx, logPrefix+": clickhouse query failed", slogx.Error(err),
			slog.String("project_id", projectID), slog.String(logFieldKey, identifier))
		telemetry.RecordError(ctx, err)
		return "", false, fmt.Errorf("%s: query failed for project %s %s=%s: %w", logPrefix, projectID, logFieldKey, identifier, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			slog.ErrorContext(ctx, logPrefix+": row iteration failed", slogx.Error(err),
				slog.String("project_id", projectID), slog.String(logFieldKey, identifier))
			telemetry.RecordError(ctx, err)
			return "", false, fmt.Errorf("%s: row iteration failed for project %s %s=%s: %w", logPrefix, projectID, logFieldKey, identifier, err)
		}
		return "", false, nil
	}

	var value string
	if err := rows.Scan(&value); err != nil {
		slog.ErrorContext(ctx, logPrefix+": scan failed", slogx.Error(err),
			slog.String("project_id", projectID), slog.String(logFieldKey, identifier))
		telemetry.RecordError(ctx, err)
		return "", false, fmt.Errorf("%s: scan failed for project %s %s=%s: %w", logPrefix, projectID, logFieldKey, identifier, err)
	}

	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, logPrefix+": row iteration failed", slogx.Error(err),
			slog.String("project_id", projectID), slog.String(logFieldKey, identifier))
		telemetry.RecordError(ctx, err)
		return "", false, fmt.Errorf("%s: row iteration failed for project %s %s=%s: %w", logPrefix, projectID, logFieldKey, identifier, err)
	}

	return value, true, nil
}

func latestProfilesQuery(projectID string) *chq.Query {
	return chq.NewQuery().
		Select(
			"id",
			"argMax(external_id, insert_time) AS external_id",
			"argMax(is_deleted, insert_time) AS is_deleted",
		).
		From("profiles").
		Where(chq.Eq("project_id", projectID)).
		GroupBy("id")
}

func latestProfileAliasesQuery(projectID string) *chq.Query {
	return chq.NewQuery().
		Select(
			"alias_id",
			"argMax(profile_id, insert_time) AS profile_id",
		).
		From("profile_aliases").
		Where(chq.Eq("project_id", projectID)).
		GroupBy("alias_id")
}

// getProfileIDForIdentifier resolves a user-facing identifier to the profile's
// profiles.id (the value stored as distinct_id for non-aliased events). The
// input may already be a profile ID, an alias ID, or a current external ID.
// If no mapping is found, returns the input unchanged so direct distinct_id
// event queries (for unmerged users or unknown identifiers) still proceed.
func (r *Reader) getProfileIDForIdentifier(ctx context.Context, projectID, distinctID string) (string, error) {
	latestProfiles := latestProfilesQuery(projectID)

	sql, args, err := chq.NewQuery().
		With("latest_profiles", latestProfiles).
		Select("id AS profile_id").
		From("latest_profiles").
		Where(chq.Eq("id", distinctID), chq.Eq("is_deleted", uint8(0))).
		Limit(1).
		Build()
	if err != nil {
		slog.ErrorContext(ctx, "getProfileIDForIdentifier: build direct profile query failed", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("distinct_id", distinctID))
		telemetry.RecordError(ctx, err)
		return "", fmt.Errorf("getProfileIDForIdentifier: build direct profile query for project %s distinct %s: %w", projectID, distinctID, err)
	}
	if profileID, ok, err := r.getSingleString(ctx, "getProfileIDForIdentifier", "distinct_id", sql, args, projectID, distinctID); err != nil {
		return "", err
	} else if ok {
		return profileID, nil
	}

	sql, args, err = chq.NewQuery().
		With("latest_profile_aliases", latestProfileAliasesQuery(projectID)).
		Select("profile_id").
		From("latest_profile_aliases").
		Where(chq.Eq("alias_id", distinctID)).
		Limit(1).
		Build()
	if err != nil {
		slog.ErrorContext(ctx, "getProfileIDForIdentifier: build alias query failed", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("distinct_id", distinctID))
		telemetry.RecordError(ctx, err)
		return "", fmt.Errorf("getProfileIDForIdentifier: build alias query for project %s distinct %s: %w", projectID, distinctID, err)
	}
	if profileID, ok, err := r.getSingleString(ctx, "getProfileIDForIdentifier", "distinct_id", sql, args, projectID, distinctID); err != nil {
		return "", err
	} else if ok {
		return profileID, nil
	}

	sql, args, err = chq.NewQuery().
		With("latest_profiles", latestProfiles).
		Select("id AS profile_id").
		From("latest_profiles").
		Where(chq.Eq("external_id", distinctID), chq.Eq("is_deleted", uint8(0))).
		Limit(1).
		Build()
	if err != nil {
		slog.ErrorContext(ctx, "getProfileIDForIdentifier: build external-id query failed", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("distinct_id", distinctID))
		telemetry.RecordError(ctx, err)
		return "", fmt.Errorf("getProfileIDForIdentifier: build external-id query for project %s distinct %s: %w", projectID, distinctID, err)
	}
	if profileID, ok, err := r.getSingleString(ctx, "getProfileIDForIdentifier", "distinct_id", sql, args, projectID, distinctID); err != nil {
		return "", err
	} else if ok {
		return profileID, nil
	}

	slog.DebugContext(ctx, "getProfileIDForIdentifier: no profile/alias/external mapping found, returning input unchanged",
		slog.String("project_id", projectID), slog.String("distinct_id", distinctID))
	return distinctID, nil
}

// getExternalIDForProfile returns the current external ID for a non-deleted
// profile. Empty string means the profile is not found, is soft-deleted, or
// has no external ID set. Soft-deleted profiles are excluded so their stale
// external IDs do not feed back into the events IN (...) filter — matching
// the is_deleted=0 guard in getProfileIDForIdentifier.
func (r *Reader) getExternalIDForProfile(ctx context.Context, projectID, profileID string) (string, error) {
	sql, args, err := chq.NewQuery().
		With("latest_profiles", latestProfilesQuery(projectID)).
		Select("external_id").
		From("latest_profiles").
		Where(chq.Eq("id", profileID), chq.Eq("is_deleted", uint8(0))).
		Limit(1).
		Build()
	if err != nil {
		slog.ErrorContext(ctx, "getExternalIDForProfile: build query failed", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("profile_id", profileID))
		telemetry.RecordError(ctx, err)
		return "", fmt.Errorf("getExternalIDForProfile: build query for project %s profile %s: %w", projectID, profileID, err)
	}

	externalID, ok, err := r.getSingleString(ctx, "getExternalIDForProfile", "profile_id", sql, args, projectID, profileID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	return externalID, nil
}

// resolveProfileIDs returns a deduplicated list of all distinct IDs to search
// in the events table for this profile: the canonical profile ID, the current
// external ID (if any), and all current alias IDs. Empty components are
// omitted, so the slice has no positional structure and may be as short as 1.
// Both projectID and distinctID must be non-empty. At the RPC boundary these are
// guaranteed by MustGetPrincipalWithProject and proto validation (required = true).
func (r *Reader) resolveProfileIDs(ctx context.Context, projectID, distinctID string) ([]string, error) {
	if projectID == "" {
		err := fmt.Errorf("resolveProfileIDs: projectID must not be empty")
		slog.ErrorContext(ctx, "resolveProfileIDs called with empty project_id", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	if distinctID == "" {
		err := fmt.Errorf("resolveProfileIDs: distinctID must not be empty")
		slog.ErrorContext(ctx, "resolveProfileIDs called with empty distinct_id", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	profileID, err := r.getProfileIDForIdentifier(ctx, projectID, distinctID)
	if err != nil {
		return nil, err
	}
	externalID, err := r.getExternalIDForProfile(ctx, projectID, profileID)
	if err != nil {
		return nil, err
	}
	aliasIDs, err := r.getAliasIDs(ctx, projectID, profileID)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, 2+len(aliasIDs))
	seen := make(map[string]struct{}, 2+len(aliasIDs))
	addID := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	addID(profileID)
	addID(externalID)
	for _, aliasID := range aliasIDs {
		addID(aliasID)
	}
	return ids, nil
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

	sql, args, err := q.OrderBy("occur_time DESC", "event_id DESC").
		Limit(int64(pageSize)).
		DisableTopKDynamicFiltering().
		Build()
	if err != nil {
		slog.ErrorContext(ctx, "GetEventExplorer: build query failed", slogx.Error(err),
			slog.String("project_id", params.ProjectID))
		telemetry.RecordError(ctx, err)
		return nil, nil, fmt.Errorf("GetEventExplorer: build query failed for project %s: %w", params.ProjectID, err)
	}

	rows, err := r.ch.Query(ctx, sql, args...)
	if err != nil {
		slog.ErrorContext(ctx, "GetEventExplorer: clickhouse query failed", slogx.Error(err),
			slog.String("project_id", params.ProjectID))
		telemetry.RecordError(ctx, err)
		return nil, nil, fmt.Errorf("GetEventExplorer: query failed for project %s: %w", params.ProjectID, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()

	var events []Event
	for rows.Next() {
		e, err := scanEvent(ctx, rows)
		if err != nil {
			slog.ErrorContext(ctx, "GetEventExplorer: scan failed", slogx.Error(err),
				slog.String("project_id", params.ProjectID))
			telemetry.RecordError(ctx, err)
			return nil, nil, fmt.Errorf("GetEventExplorer: scan failed: %w", err)
		}
		events = append(events, e)
	}

	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "GetEventExplorer: row iteration failed", slogx.Error(err),
			slog.String("project_id", params.ProjectID))
		telemetry.RecordError(ctx, err)
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

	sql, args, err := q.OrderBy("occur_time DESC", "event_id DESC").
		Limit(int64(pageSize)).
		DisableTopKDynamicFiltering().
		Build()
	if err != nil {
		slog.ErrorContext(ctx, "GetActivityFeed: build query failed", slogx.Error(err),
			slog.String("project_id", params.ProjectID))
		telemetry.RecordError(ctx, err)
		return nil, nil, fmt.Errorf("GetActivityFeed: build query failed for project %s: %w", params.ProjectID, err)
	}

	rows, err := r.ch.Query(ctx, sql, args...)
	if err != nil {
		slog.ErrorContext(ctx, "GetActivityFeed: clickhouse query failed", slogx.Error(err),
			slog.String("project_id", params.ProjectID))
		telemetry.RecordError(ctx, err)
		return nil, nil, fmt.Errorf("GetActivityFeed: query failed for project %s: %w", params.ProjectID, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()

	var events []Event
	for rows.Next() {
		e, err := scanEvent(ctx, rows)
		if err != nil {
			slog.ErrorContext(ctx, "GetActivityFeed: scan failed", slogx.Error(err),
				slog.String("project_id", params.ProjectID))
			telemetry.RecordError(ctx, err)
			return nil, nil, fmt.Errorf("GetActivityFeed: scan failed: %w", err)
		}
		events = append(events, e)
	}

	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "GetActivityFeed: row iteration failed", slogx.Error(err),
			slog.String("project_id", params.ProjectID))
		telemetry.RecordError(ctx, err)
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
func scanHeatmapDays(ctx context.Context, rows driver.Rows) ([]HeatmapDay, error) {
	var days []HeatmapDay
	for rows.Next() {
		var day string
		var cnt uint64
		if err := rows.Scan(&day, &cnt); err != nil {
			slog.ErrorContext(ctx, "scanHeatmapDays: scan failed", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return nil, fmt.Errorf("scan heatmap day: %w", err)
		}
		days = append(days, HeatmapDay{Date: day, Count: int64(cnt)})
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "scanHeatmapDays: row iteration failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
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
		slog.ErrorContext(ctx, "queryHeatmap: build query failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("queryHeatmap: build query failed for project %s: %w", projectID, err)
	}

	rows, err := r.ch.Query(ctx, sql, args...)
	if err != nil {
		slog.ErrorContext(ctx, "queryHeatmap: clickhouse query failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("queryHeatmap: query failed for project %s: %w", projectID, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()

	return scanHeatmapDays(ctx, rows)
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
		if params.TimeRange.GetFrom() == nil || params.TimeRange.GetTo() == nil {
			err := fmt.Errorf("GetActivityHeatmap: TimeRange.From and TimeRange.To must be set when TimeRange is provided")
			slog.ErrorContext(ctx, "GetActivityHeatmap called with partial TimeRange", slogx.Error(err),
				slog.String("project_id", params.ProjectID))
			telemetry.RecordError(ctx, err)
			return nil, err
		}
		from = params.TimeRange.GetFrom().AsTime()
		to = params.TimeRange.GetTo().AsTime()
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
		slog.ErrorContext(ctx, "queryProfileStats: build query failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("queryProfileStats: build query failed for project %s: %w", projectID, err)
	}

	rows, err := r.ch.Query(ctx, statSQL, statArgs...)
	if err != nil {
		slog.ErrorContext(ctx, "queryProfileStats: clickhouse query failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("queryProfileStats: query failed for project %s: %w", projectID, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()

	// ClickHouse aggregates without GROUP BY always return one row. Zero rows
	// indicates a driver bug or connection issue — return an error.
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			slog.ErrorContext(ctx, "queryProfileStats: row iteration failed", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return nil, fmt.Errorf("queryProfileStats: row iteration failed: %w", err)
		}
		err := fmt.Errorf("queryProfileStats: aggregate query returned no rows (unexpected)")
		slog.ErrorContext(ctx, "queryProfileStats: aggregate returned zero rows", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}

	var stats ProfileStats
	var totalEvents uint64
	var rawLatestProps map[string]chcol.Variant
	if err := rows.Scan(&stats.FirstSeen, &stats.LastSeen, &totalEvents, &rawLatestProps); err != nil {
		slog.ErrorContext(ctx, "queryProfileStats: scan failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("queryProfileStats: scan failed: %w", err)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "queryProfileStats: row iteration failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("queryProfileStats: row iteration failed: %w", err)
	}

	stats.TotalEvents = int64(totalEvents)
	if stats.TotalEvents == 0 {
		return nil, nil
	}

	latestProps := unwrapPropertyMap(ctx, rawLatestProps)

	stats.Browser = stringProp(latestProps, useragent.PropBrowser)
	stats.BrowserVersion = stringProp(latestProps, useragent.PropBrowserVersion)
	stats.OS = stringProp(latestProps, useragent.PropOS)
	stats.OSVersion = stringProp(latestProps, useragent.PropOSVersion)
	stats.Device = stringProp(latestProps, useragent.PropDevice)
	stats.Country = stringProp(latestProps, geo.PropCountry)
	stats.City = stringProp(latestProps, geo.PropCity)
	stats.IP = stringProp(latestProps, geo.PropIP)

	return &stats, nil
}

func stringProp(props map[string]any, key string) string {
	if v, ok := props[key].(string); ok {
		return v
	}
	return ""
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
