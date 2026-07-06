package profiles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var unrecognisedJSONTypeCounter metric.Int64Counter

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/core/profiles")
	var err error
	// Monitoring guidance: a non-zero rate on this counter means CH/driver
	// emitted a Go type the unwrap switch doesn't handle — those values are
	// dropped from Profile.Properties. Investigate the go_type label and add
	// an explicit case in unwrapJSONVariant if the new shape is legitimate.
	unrecognisedJSONTypeCounter, err = meter.Int64Counter(
		"profiles.unrecognised_json_type_total",
		metric.WithDescription("Profile JSON values whose underlying Go type the unwrap switch did not recognise. Tagged with go_type."),
	)
	if err != nil {
		// Logged at init time so operators see the registration failure on
		// startup; the global OTel meter still returns a non-nil counter so
		// subsequent .Add() calls are safe.
		slog.ErrorContext(context.Background(), "failed to register profiles.unrecognised_json_type_total counter", slogx.Error(err))
	}
}

var ErrProfileNotFound = errors.New("profile not found")

type Profile struct {
	CreateTime time.Time
	ExternalID string
	ID         string
	ProjectID  string
	Properties map[string]any
	UpdateTime time.Time
	Activity   *ProfileActivitySummary
}

type ProfileActivitySummary struct {
	FirstSeen      time.Time
	LastSeen       time.Time
	TotalEvents    int64
	Pageviews      int64
	Sessions       int64
	Browser        string
	BrowserVersion string
	OS             string
	OSVersion      string
	Device         string
	Country        string
	Region         string
	City           string
}

type ListParams struct {
	ProjectID  string
	HasCursor  bool
	CursorTime time.Time
	CursorID   string
	PageSize   int32
	Filter     chq.Condition
}

type Service struct {
	ch       driver.Conn
	pgW      *pgxpool.Pool
	write    *dbwrite.Queries
	read     *dbread.Queries
	producer *natsdeps.NATSClient
}

func NewService(pgW *pgxpool.Pool, ch driver.Conn, producer *natsdeps.NATSClient) *Service {
	var write *dbwrite.Queries
	var read *dbread.Queries
	if pgW != nil {
		write = dbwrite.New(pgW)
		read = dbread.New(pgW)
	}
	return &Service{
		ch:       ch,
		pgW:      pgW,
		write:    write,
		read:     read,
		producer: producer,
	}
}

// GetByID resolves a person by id: an identified profile row, a derived
// anonymous person (the id IS the distinct_id), or — when the id is an alias
// claimed by an identify merge — the canonical profile it was merged into, so
// pre-identify URLs and event links keep working after the merge.
func (s *Service) GetByID(ctx context.Context, projectID, id string) (Profile, error) {
	profile, err := s.getSingle(ctx, projectID, chq.Eq("p.id", id))
	if err == nil || !errors.Is(err, ErrProfileNotFound) {
		return profile, err
	}
	canonicalID, ok, err := s.resolveAliasTarget(ctx, projectID, id)
	if err != nil {
		return Profile{}, err
	}
	// The redirect is single-hop because there is no recursion here; the
	// canonicalID == id guard additionally skips a pointless re-query of the
	// same id that already missed (a self-alias).
	if !ok || canonicalID == id {
		return Profile{}, ErrProfileNotFound
	}
	return s.getSingle(ctx, projectID, chq.Eq("p.id", canonicalID))
}

func (s *Service) GetByExternalID(ctx context.Context, projectID, externalID string) (Profile, error) {
	// An empty string is never a legitimate external_id, so treat it as a miss
	// rather than a lookup: anonymous persons carry an empty external_id by
	// construction, and matching one arbitrarily would be wrong.
	if externalID == "" {
		return Profile{}, ErrProfileNotFound
	}
	return s.getSingle(ctx, projectID, chq.Eq("p.external_id", externalID))
}

func (s *Service) getSingle(ctx context.Context, projectID string, extra chq.Condition) (Profile, error) {
	if s == nil || s.ch == nil {
		return Profile{}, errors.New("profiles: clickhouse conn is nil")
	}

	sql, args, err := personsQuery(projectID).
		Where(
			chq.Eq("p.is_deleted", uint8(0)),
			extra,
		).
		OrderBy("p.update_time DESC", "p.id DESC").
		Limit(1).
		Build()
	if err != nil {
		return Profile{}, err
	}

	rows, err := s.ch.Query(ctx, sql, args...)
	if err != nil {
		return Profile{}, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Profile{}, err
		}
		return Profile{}, ErrProfileNotFound
	}

	profile, err := scanProfile(ctx, rows)
	if err != nil {
		return Profile{}, err
	}
	if err := rows.Err(); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func (s *Service) List(ctx context.Context, params ListParams) ([]Profile, error) {
	if s == nil || s.ch == nil {
		return nil, errors.New("profiles: clickhouse conn is nil")
	}

	wheres := []chq.Condition{
		chq.Eq("p.is_deleted", uint8(0)),
	}
	if params.HasCursor {
		wheres = append(wheres, chq.RawCond("(p.create_time < ? OR (p.create_time = ? AND p.id < ?))", params.CursorTime, params.CursorTime, params.CursorID))
	}
	if !params.Filter.IsZero() {
		wheres = append(wheres, params.Filter)
	}

	sql, args, err := personsQuery(params.ProjectID).
		Where(wheres...).
		OrderBy("p.create_time DESC", "p.id DESC").
		Limit(int64(params.PageSize)).
		Build()
	if err != nil {
		return nil, err
	}

	rows, err := s.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()

	items := make([]Profile, 0)
	for rows.Next() {
		profile, err := scanProfile(ctx, rows)
		if err != nil {
			return nil, err
		}
		items = append(items, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// LatestProfilesCTE projects the latest state of each profile row
// (argMax by insert_time over the ReplacingMergeTree), project-scoped.
// Exported for reuse by insights queries that resolve canonical users.
func LatestProfilesCTE(projectID string) *chq.Query {
	return chq.NewQuery().
		Select(
			"argMax(create_time, insert_time) AS create_time",
			"argMax(external_id, insert_time) AS external_id",
			"id",
			"project_id",
			"argMax(properties, insert_time) AS properties",
			"argMax(update_time, insert_time) AS update_time",
			"argMax(is_deleted, insert_time) AS is_deleted",
		).
		From("profiles").
		Where(chq.Eq("project_id", projectID)).
		GroupBy("project_id", "id")
}

// LatestProfileAliasesCTE projects the latest alias_id -> profile_id mapping,
// project-scoped. Exported for reuse by insights queries that resolve
// canonical users.
func LatestProfileAliasesCTE(projectID string) *chq.Query {
	return chq.NewQuery().
		Select(
			"alias_id",
			"argMax(profile_id, insert_time) AS profile_id",
			"project_id",
		).
		From("profile_aliases").
		Where(chq.Eq("project_id", projectID)).
		GroupBy("project_id", "alias_id")
}

// personsQuery assembles the shared skeleton of every person read (List and
// getSingle): the CTE chain that unions identified profiles with derived
// anonymous persons, joined to the per-person activity summary under the
// `p` / `activity_summary` aliases that the caller-supplied filter conditions
// bind against. Callers add WHERE / ORDER BY / LIMIT.
//
// CTE order is dependency order: claimed_ids reads the latest_* CTEs,
// anon_persons reads claimed_ids, and persons / persons_activity read
// anon_persons (plus identified_activity for the latter).
func personsQuery(projectID string) *chq.Query {
	return chq.NewQuery().
		Select(profileSelectColumns()...).
		From("persons p LEFT JOIN persons_activity activity_summary ON activity_summary.project_id = p.project_id AND activity_summary.profile_id = p.id").
		With("latest_profiles", LatestProfilesCTE(projectID)).
		With("latest_profile_aliases", LatestProfileAliasesCTE(projectID)).
		With("claimed_ids", claimedIDsCTE()).
		With("anon_persons", anonPersonsCTE(projectID)).
		With("persons", personsCTE()).
		With("identified_activity", profileActivitySummaryCTE(projectID)).
		With("persons_activity", personsActivityCTE())
}

// claimedIDsCTE projects every distinct_id already owned by the identify
// pipeline, i.e. the ids that must NOT surface as their own anonymous person:
//
//   - alias_ids — anonymous identities absorbed into an identified profile by
//     an identify merge. This is also what makes the exclusion resurrection-
//     proof: a late event for a merged distinct_id re-fires the activity MV,
//     but the alias keeps the id claimed, so nothing reappears (there is no
//     row to un-delete — persons are derived, not written).
//   - profile ids — including soft-deleted tombstones (is_deleted is not
//     consulted): a tombstoned id must stay invisible, not resurface as a
//     derived person.
//   - non-empty external_ids — post-identify events are keyed by external_id;
//     without this every identified user would grow a trait-less anonymous
//     doppelgänger.
//
// All three sets are identified-population-sized, so the anti-join stays cheap
// even when the anonymous population dominates.
func claimedIDsCTE() *chq.Query {
	return chq.NewQuery().
		Select("claimed_id").
		From(`(
SELECT alias_id AS claimed_id FROM latest_profile_aliases
UNION ALL
SELECT id AS claimed_id FROM latest_profiles
UNION ALL
SELECT external_id AS claimed_id FROM latest_profiles WHERE external_id != ''
) c`)
}

// anonPersonsCTE derives one person row per unclaimed distinct_id from the
// distinct_id_activity_states rollup — the identity materialization already
// maintained per (project_id, distinct_id) off the event stream. create_time
// is the merged first-seen (immutable except for out-of-order backfills, so
// the keyset cursor stays stable), update_time the merged last-seen. The
// activity fields are aliased exactly as profileActivitySummaryCTE's output so
// personsActivityCTE can union the two by position.
//
// Cost posture: this is a streaming GROUP BY over the states table's own
// primary key, and only the state columns a reference actually uses are read.
// It is O(project distinct_ids) per query — same class as the identified
// summary CTE — with a dedicated first-seen-ordered person index as the
// documented escape hatch if per-project cardinality outgrows it.
func anonPersonsCTE(projectID string) *chq.Query {
	return chq.NewQuery().
		Select(
			"states.project_id AS project_id",
			"states.distinct_id AS id",
			"minMerge(states.first_seen_state) AS first_seen",
			"maxMerge(states.last_seen_state) AS last_seen",
			"countMerge(states.total_events_state) AS total_events",
			"sumMerge(states.pageviews_state) AS pageviews",
			"uniqMerge(states.sessions_state) AS sessions",
			"argMaxMerge(states.latest_browser_state) AS latest_browser",
			"argMaxMerge(states.latest_browser_version_state) AS latest_browser_version",
			"argMaxMerge(states.latest_os_state) AS latest_os",
			"argMaxMerge(states.latest_os_version_state) AS latest_os_version",
			"argMaxMerge(states.latest_device_state) AS latest_device",
			"argMaxMerge(states.latest_country_state) AS latest_country",
			"argMaxMerge(states.latest_region_state) AS latest_region",
			"argMaxMerge(states.latest_city_state) AS latest_city",
		).
		From("distinct_id_activity_states states").
		Where(
			chq.Eq("states.project_id", projectID),
			// distinct_id != '': a blank-distinct_id event stream would otherwise
			// materialize a single id='' person; keep it out of the person set.
			chq.RawCond("states.distinct_id != ''"),
			chq.RawCond("states.distinct_id NOT IN claimed_ids"),
		).
		GroupBy("states.project_id", "states.distinct_id")
}

// personsCTE unions identified profiles with derived anonymous persons into
// the single relation the read queries page over. Column order mirrors
// LatestProfilesCTE's projection (UNION ALL matches by position). The
// properties literal is cast to the profiles column's exact JSON type so the
// union types line up and profile-property filters (dot-path access,
// IS_NOT_SET, numeric subcolumns) evaluate against an anonymous person the
// same way they evaluate against a trait-less identified profile.
func personsCTE() *chq.Query {
	return chq.NewQuery().
		Select("create_time", "external_id", "id", "project_id", "properties", "update_time", "is_deleted").
		From(`(
SELECT
    create_time,
    external_id,
    id,
    project_id,
    properties,
    update_time,
    is_deleted
FROM latest_profiles
UNION ALL
SELECT
    first_seen  AS create_time,
    ''          AS external_id,
    id,
    project_id,
    CAST('{}', 'JSON(max_dynamic_paths = 1000)') AS properties,
    last_seen   AS update_time,
    toUInt8(0)  AS is_deleted
FROM anon_persons
) u`)
}

// personsActivityCTE unions the identified activity summary with the derived
// anonymous persons' activity (already one row per distinct_id — no alias
// fan-out to re-aggregate). Keys are disjoint by construction: anon_persons
// excludes every claimed id, so the LEFT JOIN in personsQuery stays 1:≤1.
func personsActivityCTE() *chq.Query {
	return chq.NewQuery().
		Select(
			"project_id", "profile_id", "first_seen", "last_seen",
			"total_events", "pageviews", "sessions",
			"latest_browser", "latest_browser_version", "latest_os", "latest_os_version",
			"latest_device", "latest_country", "latest_region", "latest_city",
		).
		From(`(
SELECT
    project_id, profile_id, first_seen, last_seen,
    total_events, pageviews, sessions,
    latest_browser, latest_browser_version, latest_os, latest_os_version,
    latest_device, latest_country, latest_region, latest_city
FROM identified_activity
UNION ALL
SELECT
    project_id, id AS profile_id, first_seen, last_seen,
    total_events, pageviews, sessions,
    latest_browser, latest_browser_version, latest_os, latest_os_version,
    latest_device, latest_country, latest_region, latest_city
FROM anon_persons
) s`)
}

// resolveAliasTarget returns the canonical profile id an alias maps to,
// mirroring LatestProfileAliasesCTE's argMax-by-insert_time semantics for a
// single alias_id. ok is false when the id is not an alias.
func (s *Service) resolveAliasTarget(ctx context.Context, projectID, aliasID string) (string, bool, error) {
	rows, err := s.ch.Query(ctx,
		"SELECT argMax(profile_id, insert_time) AS profile_id FROM profile_aliases WHERE project_id = ? AND alias_id = ? GROUP BY alias_id",
		projectID, aliasID,
	)
	if err != nil {
		return "", false, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
	}()

	if !rows.Next() {
		return "", false, rows.Err()
	}
	var profileID string
	if err := rows.Scan(&profileID); err != nil {
		return "", false, err
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	return profileID, profileID != "", nil
}

// profileActivitySummaryCTE aggregates per-IDENTIFIED-profile activity
// (registered as identified_activity in personsQuery; derived anonymous
// persons carry their own single-distinct_id summary via anonPersonsCTE) by
// unioning every distinct_id that maps to the profile — profile.id,
// profile.external_id, and all alias_ids — then joining to the
// distinct_id_activity_states rollup and re-aggregating to one row per
// profile. The external_id != p.id guard avoids
// double-merging the same state when an SDK uses the same UUID for both
// columns. If a profile's external_id happens to coincide with one of its
// alias_ids, the aggregate state is merged twice: the additive aggregates
// (total_events, pageviews) double-count, while sessions (HyperLogLog) and
// the min/max/argMax columns are idempotent under repeated merging and
// remain correct.
func profileActivitySummaryCTE(projectID string) *chq.Query {
	return chq.NewQuery().
		Select(
			"identity.project_id",
			"identity.profile_id",
			"minMerge(states.first_seen_state) AS first_seen",
			"maxMerge(states.last_seen_state) AS last_seen",
			"countMerge(states.total_events_state) AS total_events",
			"sumMerge(states.pageviews_state) AS pageviews",
			"uniqMerge(states.sessions_state) AS sessions",
			"argMaxMerge(states.latest_browser_state) AS latest_browser",
			"argMaxMerge(states.latest_browser_version_state) AS latest_browser_version",
			"argMaxMerge(states.latest_os_state) AS latest_os",
			"argMaxMerge(states.latest_os_version_state) AS latest_os_version",
			"argMaxMerge(states.latest_device_state) AS latest_device",
			"argMaxMerge(states.latest_country_state) AS latest_country",
			"argMaxMerge(states.latest_region_state) AS latest_region",
			"argMaxMerge(states.latest_city_state) AS latest_city",
		).
		From(`(
SELECT
    p.project_id AS project_id,
    p.id AS profile_id,
    p.id AS distinct_id
FROM latest_profiles p
WHERE p.is_deleted = 0
UNION ALL
SELECT
    p.project_id AS project_id,
    p.id AS profile_id,
    p.external_id AS distinct_id
FROM latest_profiles p
WHERE p.is_deleted = 0
  AND p.external_id != ''
  AND p.external_id != p.id
UNION ALL
SELECT
    pa.project_id AS project_id,
    pa.profile_id AS profile_id,
    pa.alias_id AS distinct_id
FROM latest_profile_aliases pa
INNER JOIN latest_profiles p
    ON p.project_id = pa.project_id AND p.id = pa.profile_id
WHERE p.is_deleted = 0
) identity INNER JOIN distinct_id_activity_states states ON states.project_id = identity.project_id AND states.distinct_id = identity.distinct_id`).
		Where(chq.Eq("identity.project_id", projectID)).
		GroupBy("identity.project_id", "identity.profile_id")
}

func scanProfile(ctx context.Context, rows driver.Rows) (Profile, error) {
	var profile Profile
	var rawProperties chcol.JSON
	var activity ProfileActivitySummary
	var totalEvents uint64
	var pageviews uint64
	var sessions uint64
	if err := rows.Scan(
		&profile.CreateTime,
		&profile.ExternalID,
		&profile.ID,
		&profile.ProjectID,
		&rawProperties,
		&profile.UpdateTime,
		&activity.FirstSeen,
		&activity.LastSeen,
		&totalEvents,
		&pageviews,
		&sessions,
		&activity.Browser,
		&activity.BrowserVersion,
		&activity.OS,
		&activity.OSVersion,
		&activity.Device,
		&activity.Country,
		&activity.Region,
		&activity.City,
	); err != nil {
		return Profile{}, err
	}
	profile.Properties = unwrapJSONProperties(ctx, &rawProperties)
	if totalEvents > 0 {
		activity.TotalEvents = int64(totalEvents)
		activity.Pageviews = int64(pageviews)
		activity.Sessions = int64(sessions)
		profile.Activity = &activity
	}
	return profile, nil
}

// unwrapJSONProperties converts a scanned chcol.JSON value into a
// map[string]any with native Go types so downstream consumers
// (structpb.NewStruct, log/JSON marshaling) handle them uniformly.
// Always returns a non-nil map; nil input is treated as empty.
//
// Top-level keys whose value unwraps to nil are dropped from the output. This
// catches unknown-type fallbacks from unwrapJSONVariant — those return nil so
// the unrecognised value never reaches API consumers as a debug string. Note
// that nested-level nils inside `map[string]any` recursion are NOT dropped;
// chcol.NestedMap() has already removed null Variants from the input at every
// depth, so the only way a nested nil can appear is via the same unknown-type
// fallback (rare; already observable via the WARN log + counter).
func unwrapJSONProperties(ctx context.Context, j *chcol.JSON) map[string]any {
	out := make(map[string]any)
	if j == nil {
		return out
	}
	for k, v := range j.NestedMap() {
		if val := unwrapJSONValue(ctx, v); val != nil {
			out[k] = val
		}
	}
	return out
}

// unwrapJSONValue dispatches by container shape: nested maps recurse, Variant
// cells (dynamic paths) route to unwrapJSONVariant, and raw values (typed
// declared subcolumns, where the driver hands back native Go types directly)
// are wrapped in a Variant so the same type switch normalises them — otherwise
// time.Time / []*string / []chcol.JSON would leak into structpb.NewStruct and
// fail the entire profile read.
func unwrapJSONValue(ctx context.Context, v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = unwrapJSONValue(ctx, val)
		}
		return out
	case chcol.Variant:
		return unwrapJSONVariant(ctx, x)
	default:
		return unwrapJSONVariant(ctx, chcol.NewVariant(v))
	}
}

// unwrapJSONVariant materializes a single Variant cell. Native scalars pass
// through; time.Time normalizes to RFC3339Nano (so structpb consumers don't
// need a time case); array types flatten to []any. TestUnwrapJSONProperties
// pins each shape's mapping given a fixed Variant input; the catch for actual
// driver-shape drift is the testcontainer-backed integration tests
// (TestProfilePropertyKeysMV_TypeInference, TestGet_ReturnsProfile,
// TestList_BoolPropertyExcludedFromNumericFilter), which scan via the real
// driver and would fail loudly if delivered Go types changed.
//
// Unknown types drop the value (returns nil) + WARN log + counter increment
// so a CH type addition or driver change is observable rather than leaking
// debug strings to API consumers.
func unwrapJSONVariant(ctx context.Context, v chcol.Variant) any {
	switch x := v.Any().(type) {
	case nil:
		return nil
	case string, int64, float64, bool:
		return x
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []*string:
		out := make([]any, len(x))
		for i, p := range x {
			if p != nil {
				out[i] = *p
			}
		}
		return out
	case []chcol.JSON:
		out := make([]any, len(x))
		for i := range x {
			out[i] = unwrapJSONProperties(ctx, &x[i])
		}
		return out
	default:
		// WARN (not ERROR): drop-and-continue keeps the API working for the
		// rest of the profile; the counter is the page-able drift signal.
		// Bump to ERROR only if the policy becomes "fail loud" instead of
		// "degrade silently".
		goType := truncateForLabel(fmt.Sprintf("%T", x))
		err := fmt.Errorf("unrecognised JSON value type %s in profile properties", goType)
		slog.WarnContext(ctx, "unwrapJSONVariant: dropping unrecognised value",
			slogx.Error(err), slog.String("go_type", goType))
		telemetry.RecordError(ctx, err)
		unrecognisedJSONTypeCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("go_type", goType),
		))
		return nil
	}
}

// truncateForLabel bounds the cardinality of OTel attribute values derived
// from runtime type names. Parameterized or anonymous types can produce
// arbitrarily long names; capping prevents counter-series blow-out on the
// very signal designed to detect type drift.
func truncateForLabel(s string) string {
	const maxLabelLen = 64
	if len(s) <= maxLabelLen {
		return s
	}
	return s[:maxLabelLen]
}

func profileSelectColumns() []string {
	return []string{
		"p.create_time",
		"p.external_id",
		"p.id",
		"p.project_id",
		"p.properties",
		"p.update_time",
		"coalesce(activity_summary.first_seen, toDateTime64(0, 3)) AS first_seen",
		"coalesce(activity_summary.last_seen, toDateTime64(0, 3)) AS last_seen",
		"toUInt64(coalesce(activity_summary.total_events, 0)) AS total_events",
		"toUInt64(coalesce(activity_summary.pageviews, 0)) AS pageviews",
		"toUInt64(coalesce(activity_summary.sessions, 0)) AS sessions",
		"coalesce(activity_summary.latest_browser, '') AS latest_browser",
		"coalesce(activity_summary.latest_browser_version, '') AS latest_browser_version",
		"coalesce(activity_summary.latest_os, '') AS latest_os",
		"coalesce(activity_summary.latest_os_version, '') AS latest_os_version",
		"coalesce(activity_summary.latest_device, '') AS latest_device",
		"coalesce(activity_summary.latest_country, '') AS latest_country",
		"coalesce(activity_summary.latest_region, '') AS latest_region",
		"coalesce(activity_summary.latest_city, '') AS latest_city",
	}
}
