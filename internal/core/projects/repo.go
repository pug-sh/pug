package projects

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/jackc/pgx/v5"
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
	if err == nil {
		var row dbread.GetProjectAndCustomerByPrivateApiKeyRow
		if err := json.Unmarshal(data, &row); err == nil {
			return row, nil
		}
	}

	row, err := r.queries.GetProjectAndCustomerByPrivateApiKey(ctx, privateApiKey)
	if err != nil {
		if err == pgx.ErrNoRows {
			return dbread.GetProjectAndCustomerByPrivateApiKeyRow{}, pgx.ErrNoRows
		}
		return dbread.GetProjectAndCustomerByPrivateApiKeyRow{}, err
	}

	if data, err := json.Marshal(row); err == nil {
		if err := r.cache.Set(ctx, cacheKey, data, 0).Err(); err != nil {
			slog.WarnContext(ctx, "failed to cache project by private api key", slog.Any("error", err))
		}
	}

	return row, nil
}

func (r *Repo) GetProjectAndCustomerByApiKey(ctx context.Context, apiKey string) (dbread.GetProjectAndCustomerByApiKeyRow, error) {
	cacheKey := apiKeyCachePrefix + apiKey

	data, err := r.cache.Get(ctx, cacheKey).Bytes()
	if err == nil {
		var row dbread.GetProjectAndCustomerByApiKeyRow
		if err := json.Unmarshal(data, &row); err == nil {
			return row, nil
		}
	}

	row, err := r.queries.GetProjectAndCustomerByApiKey(ctx, apiKey)
	if err != nil {
		if err == pgx.ErrNoRows {
			return dbread.GetProjectAndCustomerByApiKeyRow{}, pgx.ErrNoRows
		}
		return dbread.GetProjectAndCustomerByApiKeyRow{}, err
	}

	if data, err := json.Marshal(row); err == nil {
		if err := r.cache.Set(ctx, cacheKey, data, 0).Err(); err != nil {
			slog.WarnContext(ctx, "failed to cache project by api key", slog.Any("error", err))
		}
	}

	return row, nil
}
