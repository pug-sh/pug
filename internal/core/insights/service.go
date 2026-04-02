package insights

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fivebitsio/cotton/internal/core/profiles"
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
	profiles *profiles.Repo
}

func NewService(executor *Executor, redis *redis.Client, profiles *profiles.Repo) *Service {
	return &Service{
		executor: executor,
		redis:    redis,
		profiles: profiles,
	}
}

func (s *Service) GetFilterSchema(ctx context.Context, projectID, eventKind string) (*insightsv1.GetFilterSchemaResponse, error) {
	cacheKey := "filterschema:" + projectID
	if eventKind != "" {
		cacheKey += ":" + eventKind
	}

	cached, cacheErr := s.redis.Get(ctx, cacheKey).Bytes()
	if cacheErr == nil {
		var resp insightsv1.GetFilterSchemaResponse
		if proto.Unmarshal(cached, &resp) == nil {
			return &resp, nil
		}
		slog.WarnContext(ctx, "failed to unmarshal cached filter schema, evicting", slog.String("projectID", projectID))
		s.redis.Del(ctx, cacheKey)
	} else if !errors.Is(cacheErr, redis.Nil) {
		slog.WarnContext(ctx, "redis get failed for filter schema cache", slogx.Error(cacheErr),
			slog.String("projectID", projectID))
	}

	var eventMetas []EventNameMeta
	var autoPropKeys []EventNameMeta
	var customPropKeys []EventNameMeta
	var profileProps []string

	eg, egCtx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		sql, args := BuildEventNamesQuery(projectID)
		var err error
		eventMetas, err = s.executor.QueryEventNameMetas(egCtx, sql, args)
		if err != nil {
			return fmt.Errorf("query event names: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		sql, args := BuildAutoPropertyKeysQuery(projectID, eventKind)
		var err error
		autoPropKeys, err = s.executor.QueryEventNameMetas(egCtx, sql, args)
		if err != nil {
			return fmt.Errorf("query auto property keys: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		sql, args := BuildCustomPropertyKeysQuery(projectID, eventKind)
		var err error
		customPropKeys, err = s.executor.QueryEventNameMetas(egCtx, sql, args)
		if err != nil {
			return fmt.Errorf("query custom property keys: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		var err error
		profileProps, err = s.profiles.GetPropertyKeys(egCtx, projectID)
		if err != nil {
			return fmt.Errorf("query profile property keys: %w", err)
		}
		return nil
	})

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	toEventMetas := func(rows []EventNameMeta) []*commonv1.EventNameMeta {
		out := make([]*commonv1.EventNameMeta, len(rows))
		for i, m := range rows {
			out[i] = &commonv1.EventNameMeta{Name: m.Kind, Count: m.Count, LastSeenAt: timestamppb.New(m.LastSeen)}
		}
		return out
	}
	toPropKeyMetas := func(rows []EventNameMeta) []*commonv1.PropertyKeyMeta {
		out := make([]*commonv1.PropertyKeyMeta, len(rows))
		for i, m := range rows {
			out[i] = &commonv1.PropertyKeyMeta{Name: m.Kind, Count: m.Count, LastSeenAt: timestamppb.New(m.LastSeen)}
		}
		return out
	}

	resp := &insightsv1.GetFilterSchemaResponse{
		Events:              toEventMetas(eventMetas),
		AutoPropertyKeys:    toPropKeyMetas(autoPropKeys),
		CustomPropertyKeys:  toPropKeyMetas(customPropKeys),
		ProfilePropertyKeys: profileProps,
	}

	if data, err := proto.Marshal(resp); err != nil {
		slog.ErrorContext(ctx, "failed to marshal filter schema for cache", slogx.Error(err),
			slog.String("projectID", projectID))
	} else if err := s.redis.Set(ctx, cacheKey, data, schemaCacheTTL).Err(); err != nil {
		slog.ErrorContext(ctx, "failed to cache filter schema", slogx.Error(err),
			slog.String("projectID", projectID))
	}

	return resp, nil
}

func (s *Service) GetPropertyValues(ctx context.Context, projectID, propertyKey, eventKind string, source commonv1.PropertySource) ([]string, error) {
	cacheKey := fmt.Sprintf("propvalues:%s:%d:%s:%s", projectID, source, propertyKey, eventKind)

	cached, cacheErr := s.redis.Get(ctx, cacheKey).Result()
	if cacheErr == nil {
		if cached == "" {
			return nil, nil
		}
		return strings.Split(cached, "\n"), nil
	} else if !errors.Is(cacheErr, redis.Nil) {
		slog.WarnContext(ctx, "redis get failed for property values cache", slogx.Error(cacheErr),
			slog.String("projectID", projectID), slog.String("key", propertyKey))
	}

	var values []string
	var err error

	switch source {
	case commonv1.PropertySource_PROPERTY_SOURCE_AUTO:
		sql, args := BuildPropertyValuesQuery(projectID, propertyKey, "auto_properties", eventKind)
		values, err = s.executor.QueryDistinctIDs(ctx, sql, args)
	case commonv1.PropertySource_PROPERTY_SOURCE_CUSTOM:
		sql, args := BuildPropertyValuesQuery(projectID, propertyKey, "custom_properties", eventKind)
		values, err = s.executor.QueryDistinctIDs(ctx, sql, args)
	case commonv1.PropertySource_PROPERTY_SOURCE_PROFILE:
		values, err = s.profiles.GetPropertyValues(ctx, projectID, propertyKey)
	default:
		return nil, fmt.Errorf("unsupported property source: %v", source)
	}
	if err != nil {
		return nil, fmt.Errorf("query property values: %w", err)
	}

	ttl := valuesCacheTTL
	if len(values) < 10 {
		ttl = valuesExhaustedCacheTTL
	}
	cached = strings.Join(values, "\n")
	if err := s.redis.Set(ctx, cacheKey, cached, ttl).Err(); err != nil {
		slog.ErrorContext(ctx, "failed to cache property values", slogx.Error(err),
			slog.String("projectID", projectID), slog.String("key", propertyKey))
	}

	return values, nil
}
