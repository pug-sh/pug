package projects

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/slogx"
	goredis "github.com/redis/go-redis/v9"
)

const apiKeyCachePrefix = "project:apikey:"

type Repo struct {
	queries *dbread.Queries
	cache   *goredis.Client
}

func NewRepo(queries *dbread.Queries, cache *goredis.Client) *Repo {
	return &Repo{queries: queries, cache: cache}
}

func (r *Repo) GetProjectAndCustomerByPrivateApiKey(ctx context.Context, privateApiKey string) (dbread.GetProjectAndCustomerByPrivateApiKeyRow, error) {
	cacheKey := apiKeyCachePrefix + privateApiKey

	data, err := r.cache.Get(ctx, cacheKey).Bytes()
	if err != nil && err != goredis.Nil {
		slog.WarnContext(ctx, "failed to get project by private api key from cache", slogx.Error(err))
	} else if err == nil {
		var row dbread.GetProjectAndCustomerByPrivateApiKeyRow
		if err := json.Unmarshal(data, &row); err != nil {
			slog.WarnContext(ctx, "failed to unmarshal cached project by private api key", slogx.Error(err))
		} else {
			return row, nil
		}
	}

	row, err := r.queries.GetProjectAndCustomerByPrivateApiKey(ctx, privateApiKey)
	if err != nil {
		return dbread.GetProjectAndCustomerByPrivateApiKeyRow{}, err
	}

	data, err = json.Marshal(row)
	if err != nil {
		slog.WarnContext(ctx, "failed to marshal project by private api key for caching", slogx.Error(err))
	} else if err := r.cache.Set(ctx, cacheKey, data, 0).Err(); err != nil {
		slog.WarnContext(ctx, "failed to cache project by private api key", slogx.Error(err))
	}

	return row, nil
}

func (r *Repo) GetProjectAndCustomerByApiKey(ctx context.Context, apiKey string) (dbread.GetProjectAndCustomerByApiKeyRow, error) {
	cacheKey := apiKeyCachePrefix + apiKey

	data, err := r.cache.Get(ctx, cacheKey).Bytes()
	if err != nil && err != goredis.Nil {
		slog.WarnContext(ctx, "failed to get project by api key from cache", slogx.Error(err))
	} else if err == nil {
		var row dbread.GetProjectAndCustomerByApiKeyRow
		if err := json.Unmarshal(data, &row); err != nil {
			slog.WarnContext(ctx, "failed to unmarshal cached project by api key", slogx.Error(err))
		} else {
			return row, nil
		}
	}

	row, err := r.queries.GetProjectAndCustomerByApiKey(ctx, apiKey)
	if err != nil {
		return dbread.GetProjectAndCustomerByApiKeyRow{}, err
	}

	data, err = json.Marshal(row)
	if err != nil {
		slog.WarnContext(ctx, "failed to marshal project by api key for caching", slogx.Error(err))
	} else if err := r.cache.Set(ctx, cacheKey, data, 0).Err(); err != nil {
		slog.WarnContext(ctx, "failed to cache project by api key", slogx.Error(err))
	}

	return row, nil
}
