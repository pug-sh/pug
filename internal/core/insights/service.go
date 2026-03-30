package insights

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"github.com/fivebitsio/cotton/internal/core/profiles"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/dashboard/insights/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
)

const schemaCacheTTL = 5 * time.Minute

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

func (s *Service) GetFilterSchema(ctx context.Context, projectID string) (*insightsv1.GetFilterSchemaResponse, error) {
	cacheKey := "filterschema:" + projectID

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

	var eventNames []string
	var autoPropKeys []string
	var customPropKeys []string
	var profileProps []string

	eg, egCtx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		sql, args := BuildEventNamesQuery(projectID)
		var err error
		eventNames, err = s.executor.QueryDistinctIDs(egCtx, sql, args)
		if err != nil {
			return fmt.Errorf("query event names: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		sql, args := BuildAutoPropertyKeysQuery(projectID)
		var err error
		autoPropKeys, err = s.executor.QueryDistinctIDs(egCtx, sql, args)
		if err != nil {
			return fmt.Errorf("query auto property keys: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		sql, args := BuildCustomPropertyKeysQuery(projectID)
		var err error
		customPropKeys, err = s.executor.QueryDistinctIDs(egCtx, sql, args)
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

	resp := &insightsv1.GetFilterSchemaResponse{
		EventNames:          eventNames,
		AutoPropertyKeys:    autoPropKeys,
		CustomPropertyKeys:  customPropKeys,
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
