package profiles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	workerprofilesv1 "github.com/pug-sh/pug/internal/gen/proto/workers/profiles/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var unrecognisedJSONTypeCounter metric.Int64Counter

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/core/profiles")
	var err error
	unrecognisedJSONTypeCounter, err = meter.Int64Counter(
		"profiles.unrecognised_json_type_total",
		metric.WithDescription("Profile JSON values whose underlying Go type the unwrap switch did not recognise. Tagged with go_type. Counter rate becomes a drift signal when CH adds new JSON value types."),
	)
	if err != nil {
		// init() has no context; the OTel SDK returns a usable no-op counter
		// even on validation error so .Add() will not panic, but a malformed
		// instrument name would otherwise leave operators with no startup
		// signal that observability degraded.
		slog.Error("failed to register profiles.unrecognised_json_type_total counter", slogx.Error(err))
	}
}

var ErrProfileNotFound = errors.New("profile not found")
var ErrProfileDeleteUnavailable = errors.New("profiles: delete dependencies are unavailable")

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
	producer *natsdeps.NATSClient
}

func NewService(pgW *pgxpool.Pool, ch driver.Conn, producer *natsdeps.NATSClient) *Service {
	var write *dbwrite.Queries
	if pgW != nil {
		write = dbwrite.New(pgW)
	}
	return &Service{
		ch:       ch,
		pgW:      pgW,
		write:    write,
		producer: producer,
	}
}

func (s *Service) Delete(ctx context.Context, projectID, profileID string) error {
	if s == nil || s.pgW == nil || s.write == nil || s.producer == nil {
		return ErrProfileDeleteUnavailable
	}

	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed starting delete transaction", slogx.Error(err), slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back delete transaction", slogx.Error(rollbackErr), slog.String("profile_id", profileID), slog.String("project_id", projectID))
			telemetry.RecordError(ctx, rollbackErr)
		}
	}()

	qtx := s.write.WithTx(tx)

	n, err := qtx.SoftDeleteProfileByIDAndProjectID(ctx, dbwrite.SoftDeleteProfileByIDAndProjectIDParams{
		ID:        profileID,
		ProjectID: projectID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed soft-deleting profile", slogx.Error(err), slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if n == 0 {
		return ErrProfileNotFound
	}

	deactivated, err := qtx.DeactivateDevicesByProfileID(ctx, dbwrite.DeactivateDevicesByProfileIDParams{
		ProfileID: postgres.NewText(profileID),
		ProjectID: projectID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed deactivating devices for deleted profile", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}
	slog.InfoContext(ctx, "deactivated devices for deleted profile",
		slog.Int64("count", deactivated),
		slog.String("profile_id", profileID),
		slog.String("project_id", projectID))

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing delete transaction", slogx.Error(err), slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}

	now := timestamppb.New(time.Now())
	upsertMsg := &workerprofilesv1.ProfileUpsertMessage{
		ProfileId:  proto.String(profileID),
		ProjectId:  proto.String(projectID),
		IsDeleted:  proto.Bool(true),
		UpdateTime: now,
	}
	upsertData, err := proto.Marshal(upsertMsg)
	if err != nil {
		slog.ErrorContext(ctx, "failed marshalling profile delete upsert message", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil
	}
	if err = s.producer.Publish(ctx, natsdeps.ProfileUpsertSubject, upsertData); err != nil {
		slog.ErrorContext(ctx, "failed publishing profile delete to NATS", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
	}
	return nil
}

func (s *Service) GetByID(ctx context.Context, projectID, id string) (Profile, error) {
	return s.getSingle(ctx, projectID, chq.Eq("p.id", id))
}

func (s *Service) GetByExternalID(ctx context.Context, projectID, externalID string) (Profile, error) {
	return s.getSingle(ctx, projectID, chq.Eq("p.external_id", externalID))
}

func (s *Service) getSingle(ctx context.Context, projectID string, extra chq.Condition) (Profile, error) {
	if s == nil || s.ch == nil {
		return Profile{}, errors.New("profiles: clickhouse conn is nil")
	}

	sql, args, err := chq.NewQuery().
		Select(profileSelectColumns()...).
		From("latest_profiles p LEFT JOIN profile_activity_summary activity_summary ON activity_summary.project_id = p.project_id AND activity_summary.profile_id = p.id").
		With("latest_profiles", latestProfilesCTE(projectID)).
		With("latest_profile_aliases", latestProfileAliasesCTE(projectID)).
		With("profile_activity_summary", profileActivitySummaryCTE(projectID)).
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

	sql, args, err := chq.NewQuery().
		Select(profileSelectColumns()...).
		From("latest_profiles p LEFT JOIN profile_activity_summary activity_summary ON activity_summary.project_id = p.project_id AND activity_summary.profile_id = p.id").
		With("latest_profiles", latestProfilesCTE(params.ProjectID)).
		With("latest_profile_aliases", latestProfileAliasesCTE(params.ProjectID)).
		With("profile_activity_summary", profileActivitySummaryCTE(params.ProjectID)).
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

func latestProfilesCTE(projectID string) *chq.Query {
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

func latestProfileAliasesCTE(projectID string) *chq.Query {
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
func unwrapJSONProperties(ctx context.Context, j *chcol.JSON) map[string]any {
	out := make(map[string]any)
	if j == nil {
		return out
	}
	for k, v := range j.NestedMap() {
		out[k] = unwrapJSONValue(ctx, v)
	}
	return out
}

// unwrapJSONValue dispatches by container shape: nested maps recurse, Variant
// cells (dynamic paths) route to unwrapJSONVariant, and raw values (typed
// declared subcolumns, where the driver hands back native Go types directly)
// are wrapped in a Variant so the same type switch normalises them — otherwise
// time.Time / []*string / []chcol.JSON would leak into structpb.NewStruct and
// 500 the whole profile.
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
// need a time case); array types flatten to []any. The handled shapes track
// clickhouse-go v2.46's scan behavior — see TestUnwrapJSONProperties, which
// will fail loudly on a driver upgrade that changes the delivered Go types.
// Unknown types fall through to the sentinel + WARN + counter path so a CH
// type addition or driver change is observable rather than a hard error.
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
		// WARN (not ERROR) intentionally: the sentinel keeps the API working,
		// the counter is the page-able signal, and a noisy type-drift could
		// otherwise flood error budgets. Bump to ERROR only if the policy
		// becomes "fail loud and drop the field" instead of "degrade visibly".
		goType := fmt.Sprintf("%T", x)
		err := fmt.Errorf("unrecognised JSON value type %s in profile properties", goType)
		slog.WarnContext(ctx, "unwrapJSONVariant: coercing unrecognised value to sentinel string",
			slogx.Error(err), slog.String("go_type", goType))
		telemetry.RecordError(ctx, err)
		unrecognisedJSONTypeCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("go_type", goType),
		))
		return fmt.Sprintf("<unrecognized JSON value: %s> %v", goType, x)
	}
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
