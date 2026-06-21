package insights

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"github.com/pug-sh/pug/internal/slogx"
)

const (
	schemaCacheTTL          = 5 * time.Minute
	valuesCacheTTL          = 10 * time.Minute
	valuesExhaustedCacheTTL = 1 * time.Hour
)

type Service struct {
	executor *Executor
	redis    *redis.Client
}

func NewService(executor *Executor, redis *redis.Client) *Service {
	if executor == nil {
		panic("insights: executor is nil")
	}
	if redis == nil {
		panic("insights: redis is nil")
	}
	return &Service{
		executor: executor,
		redis:    redis,
	}
}

func variantTypeToPropertyValueType(raw string) commonv1.PropertyValueType {
	switch raw {
	case "":
		return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_UNSPECIFIED
	case "String":
		return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_STRING
	case "Int64":
		return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER
	case "Float64":
		return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_FLOAT
	case "Bool":
		return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN
	}
	// Match any DateTime64(precision) so a future precision change in the
	// migration doesn't silently demote dates to OTHER. The pin tests catch
	// the migration drift independently.
	if strings.HasPrefix(raw, "DateTime64(") {
		return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_DATETIME
	}
	return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_OTHER
}

func normalizeAllowedTypes(in []commonv1.PropertyValueType) []commonv1.PropertyValueType {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[commonv1.PropertyValueType]struct{}, len(in))
	out := make([]commonv1.PropertyValueType, 0, len(in))
	for _, t := range in {
		if t == commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_UNSPECIFIED {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return out
}

// promotedAutoDimValueType is the variant type name reported for every
// rollup-backed promoted breakdown dimension. All materializedDims are
// string-typed events columns (String / LowCardinality(String)), so they
// surface as string properties.
const promotedAutoDimValueType = "String"

// mergePromotedAutoDimensions injects the rollup-backed promoted breakdown
// dimensions (materializedDims: browser, country, region, ...) into the
// discovered auto-property keys. These dimensions live in dedicated events
// columns, not the auto_properties map, so the property_keys MV never observes
// them — without this they are missing from the filter/breakdown picker even
// though the query engine fully supports them. Counts and last-seen come from
// the event rollup when available; a dimension with no rollup rows — or when the
// caller degraded a failed/absent rollup query to a nil result — is still
// surfaced (count 0), so discovery never depends on the rollup being populated
// or healthy. The combined list is re-sorted by count so the busiest properties
// stay on top.
func mergePromotedAutoDimensions(discovered, rollup []AggregateKeyMeta) []AggregateKeyMeta {
	byKey := make(map[string]AggregateKeyMeta, len(rollup))
	for _, r := range rollup {
		byKey[r.Key] = r
	}

	out := make([]AggregateKeyMeta, 0, len(discovered)+len(materializedDims))
	for _, k := range discovered {
		// A promoted dim should never appear in property_keys (it is stripped
		// from the map at ingest); drop any such duplicate in favor of the
		// authoritative rollup-sourced entry appended below.
		if isMaterializedDim(k.Key) {
			continue
		}
		out = append(out, k)
	}
	for _, dim := range materializedDims {
		meta := AggregateKeyMeta{Key: dim, ValueType: promotedAutoDimValueType}
		if r, ok := byKey[dim]; ok {
			meta.Count = r.Count
			meta.LastSeen = r.LastSeen
		}
		out = append(out, meta)
	}

	slices.SortStableFunc(out, func(a, b AggregateKeyMeta) int {
		// Count descending (busiest first), then key ascending as a stable,
		// deterministic tie-break — see executor.go for the same idiom.
		if c := cmp.Compare(b.Count, a.Count); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	return out
}

func filterAggregateKeysByType(rows []AggregateKeyMeta, allowed []commonv1.PropertyValueType) []AggregateKeyMeta {
	allowed = normalizeAllowedTypes(allowed)
	if len(allowed) == 0 {
		return rows
	}
	allowedSet := make(map[commonv1.PropertyValueType]struct{}, len(allowed))
	for _, t := range allowed {
		allowedSet[t] = struct{}{}
	}

	out := make([]AggregateKeyMeta, 0, len(rows))
	for _, row := range rows {
		if _, ok := allowedSet[variantTypeToPropertyValueType(row.ValueType)]; ok {
			out = append(out, row)
		}
	}
	return out
}

// lastSeenTimestamp converts a key's last-seen instant to a protobuf timestamp,
// returning nil for the zero time. A count-0 promoted dimension (one with no
// rollup rows) has no genuine last-seen instant; emitting nil keeps last_seen_at
// absent rather than serializing the Go zero time as a misleading 0001-01-01.
func lastSeenTimestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func (s *Service) GetFilterSchema(ctx context.Context, projectID, eventKind string, allowedTypes []commonv1.PropertyValueType) (*commonv1.GetFilterSchemaResponse, error) {
	allowedTypes = normalizeAllowedTypes(allowedTypes)
	cacheKey := "filterschema:" + projectID
	if eventKind != "" {
		cacheKey += ":" + eventKind
	}
	if len(allowedTypes) > 0 {
		parts := make([]string, len(allowedTypes))
		for i, t := range allowedTypes {
			parts[i] = t.String()
		}
		cacheKey += ":types=" + strings.Join(parts, ",")
	}

	cachedSchema, cacheErr := s.redis.Get(ctx, cacheKey).Bytes()
	if cacheErr == nil {
		var resp commonv1.GetFilterSchemaResponse
		if err := proto.Unmarshal(cachedSchema, &resp); err != nil {
			slog.WarnContext(ctx, "failed to unmarshal cached filter schema, evicting",
				slogx.Error(err), slog.String("project_id", projectID))
			if delErr := s.redis.Del(ctx, cacheKey).Err(); delErr != nil {
				// Eviction failure is Error (not Warn): the corrupt blob will
				// be re-read by the next request and trigger another failed
				// eviction. Without escalation, the loop is silent at Warn.
				slog.ErrorContext(ctx, "failed to evict corrupt filter schema cache",
					slogx.Error(delErr), slog.String("project_id", projectID))
				telemetry.RecordError(ctx, delErr)
			}
		} else {
			return &resp, nil
		}
	} else if !errors.Is(cacheErr, redis.Nil) {
		slog.WarnContext(ctx, "redis get failed for filter schema cache", slogx.Error(cacheErr),
			slog.String("project_id", projectID))
	}

	var eventMetas []AggregateKeyMeta
	var autoPropKeys []AggregateKeyMeta
	var promotedAutoKeys []AggregateKeyMeta
	var promotedDegraded bool
	var customPropKeys []AggregateKeyMeta
	var profilePropKeys []AggregateKeyMeta

	eg, egCtx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		sql, args, err := BuildEventNamesQuery(projectID)
		if err != nil {
			slog.ErrorContext(egCtx, "build event names query failed", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(egCtx, err)
			return fmt.Errorf("build event names query: %w", err)
		}
		eventMetas, err = s.executor.QueryAggregateKeys(egCtx, projectID, sql, args)
		if err != nil {
			return fmt.Errorf("query event names: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		sql, args, err := BuildAutoPropertyKeysQuery(projectID, eventKind)
		if err != nil {
			slog.ErrorContext(egCtx, "build auto property keys query failed", slogx.Error(err),
				slog.String("project_id", projectID), slog.String("event_kind", eventKind))
			telemetry.RecordError(egCtx, err)
			return fmt.Errorf("build auto property keys query: %w", err)
		}
		autoPropKeys, err = s.executor.QueryAggregateKeys(egCtx, projectID, sql, args)
		if err != nil {
			return fmt.Errorf("query auto property keys: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		sql, args, err := BuildPromotedAutoPropertyKeysQuery(projectID, eventKind)
		if err != nil {
			slog.ErrorContext(egCtx, "build promoted auto property keys query failed", slogx.Error(err),
				slog.String("project_id", projectID), slog.String("event_kind", eventKind))
			telemetry.RecordError(egCtx, err)
			return fmt.Errorf("build promoted auto property keys query: %w", err)
		}
		keys, err := s.executor.QueryAggregateKeys(egCtx, projectID, sql, args)
		if err != nil {
			// Promoted dimensions are an enhancement sourced from the
			// dashboard_event_rollup_daily fast-path table, which may be absent
			// (migration 006 not yet applied) or transiently unavailable. Unlike
			// the foundational event_names / property_keys fetches, a rollup
			// failure must NOT fail the whole schema: degrade to count-0
			// dimensions — mergePromotedAutoDimensions still lists every dim — so
			// the picker stays populated. QueryAggregateKeys already logs+records
			// non-context query failures, so this is only a disposition line, not a
			// second RecordError; swallowing keeps egCtx alive for the sibling
			// fetches. (A context cancellation here means a sibling already failed;
			// its error surfaces from eg.Wait.)
			slog.WarnContext(egCtx, "promoted auto dimensions unavailable; serving filter schema without rollup counts",
				slogx.Error(err), slog.String("project_id", projectID), slog.String("event_kind", eventKind))
			promotedDegraded = true
			return nil
		}
		promotedAutoKeys = keys
		return nil
	})
	eg.Go(func() error {
		sql, args, err := BuildCustomPropertyKeysQuery(projectID, eventKind)
		if err != nil {
			slog.ErrorContext(egCtx, "build custom property keys query failed", slogx.Error(err),
				slog.String("project_id", projectID), slog.String("event_kind", eventKind))
			telemetry.RecordError(egCtx, err)
			return fmt.Errorf("build custom property keys query: %w", err)
		}
		customPropKeys, err = s.executor.QueryAggregateKeys(egCtx, projectID, sql, args)
		if err != nil {
			return fmt.Errorf("query custom property keys: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		sql, args, err := BuildProfilePropertyKeysQuery(projectID)
		if err != nil {
			slog.ErrorContext(egCtx, "build profile property keys query failed", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(egCtx, err)
			return fmt.Errorf("build profile property keys query: %w", err)
		}
		keys, err := s.executor.QueryAggregateKeys(egCtx, projectID, sql, args)
		if err != nil {
			return fmt.Errorf("query profile property keys: %w", err)
		}
		profilePropKeys = keys
		return nil
	})

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	autoPropKeys = mergePromotedAutoDimensions(autoPropKeys, promotedAutoKeys)
	autoPropKeys = filterAggregateKeysByType(autoPropKeys, allowedTypes)
	customPropKeys = filterAggregateKeysByType(customPropKeys, allowedTypes)
	profilePropKeys = filterAggregateKeysByType(profilePropKeys, allowedTypes)

	toEventMetas := func(rows []AggregateKeyMeta) []*commonv1.EventNameMeta {
		out := make([]*commonv1.EventNameMeta, len(rows))
		for i, m := range rows {
			key := m.Key
			count := m.Count
			out[i] = &commonv1.EventNameMeta{Name: proto.String(key), Count: &count, LastSeenAt: lastSeenTimestamp(m.LastSeen)}
		}
		return out
	}
	toPropKeyMetas := func(rows []AggregateKeyMeta) []*commonv1.PropertyKeyMeta {
		out := make([]*commonv1.PropertyKeyMeta, len(rows))
		for i, m := range rows {
			key := m.Key
			count := m.Count
			valueType := variantTypeToPropertyValueType(m.ValueType)
			out[i] = &commonv1.PropertyKeyMeta{Name: proto.String(key), Count: &count, LastSeenAt: lastSeenTimestamp(m.LastSeen), ValueType: &valueType}
		}
		return out
	}

	resp := &commonv1.GetFilterSchemaResponse{
		Events:              toEventMetas(eventMetas),
		AutoPropertyKeys:    toPropKeyMetas(autoPropKeys),
		CustomPropertyKeys:  toPropKeyMetas(customPropKeys),
		ProfilePropertyKeys: toPropKeyMetas(profilePropKeys),
	}

	// A degraded schema — promoted dimensions served at count 0 because the rollup
	// fast-path was unavailable — must not be cached. Caching it would freeze the
	// count-0 dims for the full schemaCacheTTL, amplifying a transient rollup blip
	// into minutes of stale zeros (and a corrupted count-sort order) for every
	// reader of this project. Skip the write and recompute next request, which
	// picks up a recovered rollup immediately.
	if promotedDegraded {
		return resp, nil
	}

	if data, err := proto.Marshal(resp); err != nil {
		slog.ErrorContext(ctx, "failed to marshal filter schema for cache", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
	} else if err := s.redis.Set(ctx, cacheKey, data, schemaCacheTTL).Err(); err != nil {
		slog.WarnContext(ctx, "failed to cache filter schema", slogx.Error(err),
			slog.String("project_id", projectID))
	}

	return resp, nil
}

func (s *Service) GetPropertyValues(ctx context.Context, projectID, propertyKey, eventKind string, source commonv1.PropertySource) ([]string, error) {
	cacheKey := fmt.Sprintf("propvalues:%s:%s:%s:%s", projectID, source.String(), propertyKey, eventKind)

	cached, cacheErr := s.redis.Get(ctx, cacheKey).Bytes()
	if cacheErr == nil {
		var vals []string
		if err := json.Unmarshal(cached, &vals); err != nil {
			slog.WarnContext(ctx, "failed to unmarshal cached property values, evicting",
				slogx.Error(err), slog.String("project_id", projectID), slog.String("key", propertyKey))
			if delErr := s.redis.Del(ctx, cacheKey).Err(); delErr != nil {
				// See note above on the filter-schema eviction path: corrupt
				// blob persists if Del fails, so escalate to Error.
				slog.ErrorContext(ctx, "failed to evict corrupt property values cache",
					slogx.Error(delErr), slog.String("project_id", projectID))
				telemetry.RecordError(ctx, delErr)
			}
		} else {
			return vals, nil
		}
	} else if !errors.Is(cacheErr, redis.Nil) {
		slog.WarnContext(ctx, "redis get failed for property values cache", slogx.Error(cacheErr),
			slog.String("project_id", projectID), slog.String("key", propertyKey))
	}

	var values []string
	var err error

	switch source {
	case commonv1.PropertySource_PROPERTY_SOURCE_AUTO:
		sql, args, buildErr := BuildAutoPropertyValuesQuery(projectID, propertyKey, eventKind)
		if buildErr != nil {
			slog.ErrorContext(ctx, "build auto property values query failed", slogx.Error(buildErr),
				slog.String("project_id", projectID), slog.String("property_key", propertyKey), slog.String("event_kind", eventKind))
			telemetry.RecordError(ctx, buildErr)
			return nil, fmt.Errorf("build property values query: %w", buildErr)
		}
		values, err = s.executor.QueryStringColumn(ctx, projectID, sql, args)
	case commonv1.PropertySource_PROPERTY_SOURCE_CUSTOM:
		sql, args, buildErr := BuildCustomPropertyValuesQuery(projectID, propertyKey, eventKind)
		if buildErr != nil {
			slog.ErrorContext(ctx, "build custom property values query failed", slogx.Error(buildErr),
				slog.String("project_id", projectID), slog.String("property_key", propertyKey), slog.String("event_kind", eventKind))
			telemetry.RecordError(ctx, buildErr)
			return nil, fmt.Errorf("build property values query: %w", buildErr)
		}
		values, err = s.executor.QueryStringColumn(ctx, projectID, sql, args)
	case commonv1.PropertySource_PROPERTY_SOURCE_PROFILE:
		sql, args, buildErr := BuildProfilePropertyValuesQuery(projectID, propertyKey)
		if buildErr != nil {
			slog.ErrorContext(ctx, "build profile property values query failed", slogx.Error(buildErr),
				slog.String("project_id", projectID), slog.String("property_key", propertyKey))
			telemetry.RecordError(ctx, buildErr)
			return nil, fmt.Errorf("build profile property values query: %w", buildErr)
		}
		values, err = s.executor.QueryStringColumn(ctx, projectID, sql, args)
	default:
		err := fmt.Errorf("unsupported property source: %v", source)
		slog.ErrorContext(ctx, "unsupported property source reached service default", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("property_key", propertyKey),
			slog.String("source", source.String()))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("query property values: %w", err)
	}

	ttl := valuesCacheTTL
	if len(values) < PropertyValuesLimit {
		ttl = valuesExhaustedCacheTTL
	}
	if data, err := json.Marshal(values); err != nil {
		slog.ErrorContext(ctx, "failed to marshal property values for cache", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("key", propertyKey))
		telemetry.RecordError(ctx, err)
	} else if err := s.redis.Set(ctx, cacheKey, data, ttl).Err(); err != nil {
		slog.WarnContext(ctx, "failed to cache property values", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("key", propertyKey))
	}

	return values, nil
}
