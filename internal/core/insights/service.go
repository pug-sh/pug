package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
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

func (s *Service) GetFilterSchema(
	ctx context.Context,
	projectID, eventKind string,
	allowedTypes []commonv1.PropertyValueType,
) (*insightsv1.GetFilterSchemaResponse, error) {
	allowedTypes = normalizeAllowedTypes(allowedTypes)

	cacheKey := "filterschema:" + projectID
	if eventKind != "" {
		cacheKey += ":" + eventKind
	}
	if len(allowedTypes) > 0 {
		parts := make([]string, len(allowedTypes))
		for i, t := range allowedTypes {
			parts[i] = strconv.Itoa(int(t))
		}
		cacheKey += ":types=" + strings.Join(parts, ",")
	}

	cachedSchema, cacheErr := s.redis.Get(ctx, cacheKey).Bytes()
	if cacheErr == nil {
		var resp insightsv1.GetFilterSchemaResponse
		if err := proto.Unmarshal(cachedSchema, &resp); err != nil {
			slog.WarnContext(ctx, "failed to unmarshal cached filter schema, evicting",
				slogx.Error(err), slog.String("project_id", projectID))
			if delErr := s.redis.Del(ctx, cacheKey).Err(); delErr != nil {
				slog.WarnContext(ctx, "failed to evict corrupt filter schema cache",
					slogx.Error(delErr), slog.String("project_id", projectID))
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

	toEventMetas := func(rows []AggregateKeyMeta) []*commonv1.EventNameMeta {
		out := make([]*commonv1.EventNameMeta, len(rows))
		for i, m := range rows {
			key := m.Key
			count := m.Count
			out[i] = &commonv1.EventNameMeta{Name: proto.String(key), Count: &count, LastSeenAt: timestamppb.New(m.LastSeen)}
		}
		return out
	}
	toPropKeyMetas := func(rows []AggregateKeyMeta) []*commonv1.PropertyKeyMeta {
		out := make([]*commonv1.PropertyKeyMeta, len(rows))
		for i, m := range rows {
			key := m.Key
			count := m.Count
			vt := variantTypeToPropertyValueType(m.ValueType)
			out[i] = &commonv1.PropertyKeyMeta{
				Name:       proto.String(key),
				Count:      &count,
				LastSeenAt: timestamppb.New(m.LastSeen),
				ValueType:  &vt,
			}
		}
		return out
	}

	resp := &insightsv1.GetFilterSchemaResponse{
		Events:              toEventMetas(eventMetas),
		AutoPropertyKeys:    toPropKeyMetas(filterAggregateKeysByType(autoPropKeys, allowedTypes)),
		CustomPropertyKeys:  toPropKeyMetas(filterAggregateKeysByType(customPropKeys, allowedTypes)),
		ProfilePropertyKeys: toPropKeyMetas(filterAggregateKeysByType(profilePropKeys, allowedTypes)),
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
				slog.WarnContext(ctx, "failed to evict corrupt property values cache",
					slogx.Error(delErr), slog.String("project_id", projectID))
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

// normalizeAllowedTypes sorts and dedupes a list of PropertyValueType values
// and drops UNSPECIFIED. Returns nil if no meaningful types remain — the canonical
// "no filter" form.
func normalizeAllowedTypes(types []commonv1.PropertyValueType) []commonv1.PropertyValueType {
	if len(types) == 0 {
		return nil
	}
	seen := make(map[commonv1.PropertyValueType]struct{}, len(types))
	out := make([]commonv1.PropertyValueType, 0, len(types))
	for _, t := range types {
		if t == commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_UNSPECIFIED {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) == 0 {
		return nil
	}
	return out
}

// filterAggregateKeysByType returns only rows whose ValueType maps to a proto
// enum value in the allowed set. allowedTypes is assumed normalized (sorted, no
// UNSPECIFIED, no duplicates). nil/empty allowedTypes means "no filter".
func filterAggregateKeysByType(rows []AggregateKeyMeta, allowedTypes []commonv1.PropertyValueType) []AggregateKeyMeta {
	if len(allowedTypes) == 0 {
		return rows
	}
	allowed := make(map[commonv1.PropertyValueType]struct{}, len(allowedTypes))
	for _, t := range allowedTypes {
		allowed[t] = struct{}{}
	}
	out := make([]AggregateKeyMeta, 0, len(rows))
	for _, row := range rows {
		protoType := variantTypeToPropertyValueType(row.ValueType)
		if _, ok := allowed[protoType]; ok {
			out = append(out, row)
		}
	}
	return out
}

// variantTypeToPropertyValueType maps the string emitted by the property_keys
// MV's value_type column into the proto PropertyValueType enum.
//
// The custom_properties MV emits CH-native variant type names from variantType():
//
//	"String", "Int64", "Float64", "Bool", "DateTime64(3)".
//
// The profile MV emits JSON-shape names from its hand-built multiIf:
//
//	"String", "Number", "Bool", "Object", "Array", "None".
//
// The auto_properties MV emits the constant "String".
//
// "" (empty) maps to UNSPECIFIED — used when no value_type was scanned (e.g.
// event_names rows, which have no type column). Anything else not matched
// explicitly maps to OTHER (the catch-all for non-primitive shapes).
func variantTypeToPropertyValueType(s string) commonv1.PropertyValueType {
	switch s {
	case "":
		return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_UNSPECIFIED
	case "String":
		return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_STRING
	case "Int64", "Float64", "Number":
		return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_NUMBER
	case "Bool":
		return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_BOOLEAN
	// "DateTime64(3)" must match the Variant precision declared in
	// schema/clickhouse/migrations/005_typed_custom_properties.sql.
	// If the migration changes precision, update this case.
	case "DateTime64(3)":
		return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_DATETIME
	default:
		return commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_OTHER
	}
}
