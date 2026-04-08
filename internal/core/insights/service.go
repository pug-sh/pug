package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

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

func (s *Service) GetFilterSchema(ctx context.Context, projectID, eventKind string) (*insightsv1.GetFilterSchemaResponse, error) {
	cacheKey := "filterschema:" + projectID
	if eventKind != "" {
		cacheKey += ":" + eventKind
	}

	cachedSchema, cacheErr := s.redis.Get(ctx, cacheKey).Bytes()
	if cacheErr == nil {
		var resp insightsv1.GetFilterSchemaResponse
		if err := proto.Unmarshal(cachedSchema, &resp); err != nil {
			slog.WarnContext(ctx, "failed to unmarshal cached filter schema, evicting",
				slogx.Error(err), slog.String("projectID", projectID))
			if delErr := s.redis.Del(ctx, cacheKey).Err(); delErr != nil {
				slog.WarnContext(ctx, "failed to evict corrupt filter schema cache",
					slogx.Error(delErr), slog.String("projectID", projectID))
			}
		} else {
			return &resp, nil
		}
	} else if !errors.Is(cacheErr, redis.Nil) {
		slog.WarnContext(ctx, "redis get failed for filter schema cache", slogx.Error(cacheErr),
			slog.String("projectID", projectID))
	}

	var eventMetas []AggregateKeyMeta
	var autoPropKeys []AggregateKeyMeta
	var customPropKeys []AggregateKeyMeta
	var profilePropKeys []AggregateKeyMeta

	eg, egCtx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		sql, args, err := BuildEventNamesQuery(projectID)
		if err != nil {
			return fmt.Errorf("build event names query: %w", err)
		}
		eventMetas, err = s.executor.QueryAggregateKeys(egCtx, sql, args)
		if err != nil {
			return fmt.Errorf("query event names: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		sql, args, err := BuildAutoPropertyKeysQuery(projectID, eventKind)
		if err != nil {
			return fmt.Errorf("build auto property keys query: %w", err)
		}
		autoPropKeys, err = s.executor.QueryAggregateKeys(egCtx, sql, args)
		if err != nil {
			return fmt.Errorf("query auto property keys: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		sql, args, err := BuildCustomPropertyKeysQuery(projectID, eventKind)
		if err != nil {
			return fmt.Errorf("build custom property keys query: %w", err)
		}
		customPropKeys, err = s.executor.QueryAggregateKeys(egCtx, sql, args)
		if err != nil {
			return fmt.Errorf("query custom property keys: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		sql, args, err := BuildProfilePropertyKeysQuery(projectID)
		if err != nil {
			return fmt.Errorf("build profile property keys query: %w", err)
		}
		var err2 error
		profilePropKeys, err2 = s.executor.QueryAggregateKeys(egCtx, sql, args)
		if err2 != nil {
			return fmt.Errorf("query profile property keys: %w", err2)
		}
		return nil
	})

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	toEventMetas := func(rows []AggregateKeyMeta) []*commonv1.EventNameMeta {
		out := make([]*commonv1.EventNameMeta, len(rows))
		for i, m := range rows {
			out[i] = &commonv1.EventNameMeta{Name: m.Key, Count: m.Count, LastSeenAt: timestamppb.New(m.LastSeen)}
		}
		return out
	}
	toPropKeyMetas := func(rows []AggregateKeyMeta) []*commonv1.PropertyKeyMeta {
		out := make([]*commonv1.PropertyKeyMeta, len(rows))
		for i, m := range rows {
			out[i] = &commonv1.PropertyKeyMeta{Name: m.Key, Count: m.Count, LastSeenAt: timestamppb.New(m.LastSeen)}
		}
		return out
	}

	resp := &insightsv1.GetFilterSchemaResponse{
		Events:              toEventMetas(eventMetas),
		AutoPropertyKeys:    toPropKeyMetas(autoPropKeys),
		CustomPropertyKeys:  toPropKeyMetas(customPropKeys),
		ProfilePropertyKeys: toPropKeyMetas(profilePropKeys),
	}

	if data, err := proto.Marshal(resp); err != nil {
		slog.ErrorContext(ctx, "failed to marshal filter schema for cache", slogx.Error(err),
			slog.String("projectID", projectID))
	} else if err := s.redis.Set(ctx, cacheKey, data, schemaCacheTTL).Err(); err != nil {
		slog.WarnContext(ctx, "failed to cache filter schema", slogx.Error(err),
			slog.String("projectID", projectID))
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
				slogx.Error(err), slog.String("projectID", projectID), slog.String("key", propertyKey))
			if delErr := s.redis.Del(ctx, cacheKey).Err(); delErr != nil {
				slog.WarnContext(ctx, "failed to evict corrupt property values cache",
					slogx.Error(delErr), slog.String("projectID", projectID))
			}
		} else {
			return vals, nil
		}
	} else if !errors.Is(cacheErr, redis.Nil) {
		slog.WarnContext(ctx, "redis get failed for property values cache", slogx.Error(cacheErr),
			slog.String("projectID", projectID), slog.String("key", propertyKey))
	}

	var values []string
	var err error

	switch source {
	case commonv1.PropertySource_PROPERTY_SOURCE_AUTO:
		sql, args, buildErr := BuildAutoPropertyValuesQuery(projectID, propertyKey, eventKind)
		if buildErr != nil {
			return nil, fmt.Errorf("build property values query: %w", buildErr)
		}
		values, err = s.executor.QueryStringColumn(ctx, sql, args)
	case commonv1.PropertySource_PROPERTY_SOURCE_CUSTOM:
		sql, args, buildErr := BuildCustomPropertyValuesQuery(projectID, propertyKey, eventKind)
		if buildErr != nil {
			return nil, fmt.Errorf("build property values query: %w", buildErr)
		}
		values, err = s.executor.QueryStringColumn(ctx, sql, args)
	case commonv1.PropertySource_PROPERTY_SOURCE_PROFILE:
		sql, args, buildErr := BuildProfilePropertyValuesQuery(projectID, propertyKey)
		if buildErr != nil {
			return nil, fmt.Errorf("build profile property values query: %w", buildErr)
		}
		values, err = s.executor.QueryStringColumn(ctx, sql, args)
	default:
		return nil, fmt.Errorf("unsupported property source: %v", source)
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
			slog.String("projectID", projectID), slog.String("key", propertyKey))
	} else if err := s.redis.Set(ctx, cacheKey, data, ttl).Err(); err != nil {
		slog.WarnContext(ctx, "failed to cache property values", slogx.Error(err),
			slog.String("projectID", projectID), slog.String("key", propertyKey))
	}

	return values, nil
}
