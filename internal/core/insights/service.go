package insights

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
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

func NewService(ch driver.Conn, redis *redis.Client, profiles *profiles.Repo) *Service {
	return &Service{
		executor: NewExecutor(ch),
		redis:    redis,
		profiles: profiles,
	}
}

func (s *Service) GetFilterSchema(ctx context.Context, projectID string) (*insightsv1.GetFilterSchemaResponse, error) {
	cacheKey := "filterschema:" + projectID

	cached, cacheErr := s.redis.Get(ctx, cacheKey).Bytes()
	if cacheErr != nil && !errors.Is(cacheErr, redis.Nil) {
		slog.WarnContext(ctx, "redis get failed for filter schema cache", slogx.Error(cacheErr),
			slog.String("projectID", projectID))
	} else if cacheErr == nil {
		var resp insightsv1.GetFilterSchemaResponse
		if unmarshalErr := proto.Unmarshal(cached, &resp); unmarshalErr == nil {
			return &resp, nil
		} else {
			slog.WarnContext(ctx, "failed to unmarshal cached filter schema, falling through", slogx.Error(unmarshalErr),
				slog.String("projectID", projectID))
		}
	}

	var eventNames []string
	var autoPropKeys []string
	var customPropKeys []string
	var profileProps []string

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		var err error
		eventNames, err = s.queryEventNames(ctx, projectID)
		return err
	})
	eg.Go(func() error {
		var err error
		autoPropKeys, err = s.queryAutoPropertyKeys(ctx, projectID)
		return err
	})
	eg.Go(func() error {
		var err error
		customPropKeys, err = s.queryCustomPropertyKeys(ctx, projectID)
		return err
	})
	eg.Go(func() error {
		var err error
		profileProps, err = s.profiles.GetPropertyKeys(ctx, projectID)
		return err
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

	if data, marshalErr := proto.Marshal(resp); marshalErr != nil {
		slog.WarnContext(ctx, "failed to marshal filter schema for cache", slogx.Error(marshalErr),
			slog.String("projectID", projectID))
	} else {
		if err := s.redis.Set(ctx, cacheKey, data, schemaCacheTTL).Err(); err != nil {
			slog.WarnContext(ctx, "failed to cache filter schema", slogx.Error(err),
				slog.String("projectID", projectID))
		}
	}

	return resp, nil
}

func (s *Service) queryEventNames(ctx context.Context, projectID string) ([]string, error) {
	sql, args := BuildEventNamesQuery(projectID)
	return s.executor.QueryDistinctIDs(ctx, sql, args)
}

func (s *Service) queryAutoPropertyKeys(ctx context.Context, projectID string) ([]string, error) {
	sql, args := BuildAutoPropertyKeysQuery(projectID)
	return s.executor.QueryDistinctIDs(ctx, sql, args)
}

func (s *Service) queryCustomPropertyKeys(ctx context.Context, projectID string) ([]string, error) {
	sql, args := BuildCustomPropertyKeysQuery(projectID)
	return s.executor.QueryDistinctIDs(ctx, sql, args)
}
