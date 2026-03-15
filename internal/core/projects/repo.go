package projects

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/slogx"
	goredis "github.com/redis/go-redis/v9"
)

const (
	privateKeyCachePrefix = "project:prvkey:"
	publicKeyCachePrefix  = "project:pubkey:"
	apiKeyCacheTTL        = 30 * 24 * time.Hour
)

type Repo struct {
	queries *dbread.Queries
	cache   *goredis.Client
}

func NewRepo(queries *dbread.Queries, cache *goredis.Client) *Repo {
	return &Repo{queries: queries, cache: cache}
}

func (r *Repo) GetProjectAndCustomerByPrivateApiKey(ctx context.Context, privateApiKey string) (dbread.GetProjectAndCustomerByPrivateApiKeyRow, error) {
	cacheKey := privateKeyCachePrefix + privateApiKey

	data, err := r.cache.Get(ctx, cacheKey).Bytes()
	if err != nil && !errors.Is(err, goredis.Nil) {
		slog.WarnContext(ctx, "failed to get project by private api key from cache", slogx.Error(err))
	} else if err == nil {
		var row dbread.GetProjectAndCustomerByPrivateApiKeyRow
		if err := json.Unmarshal(data, &row); err != nil {
			slog.WarnContext(ctx, "failed to unmarshal cached project by private api key, deleting corrupt entry", slogx.Error(err))
			r.cache.Del(ctx, cacheKey)
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
	} else if err := r.cache.Set(ctx, cacheKey, data, apiKeyCacheTTL).Err(); err != nil {
		slog.WarnContext(ctx, "failed to cache project by private api key", slogx.Error(err))
	}

	return row, nil
}

func (r *Repo) InvalidateProjectKeys(ctx context.Context, privateKey, publicKey string) {
	keys := []string{
		privateKeyCachePrefix + privateKey,
		publicKeyCachePrefix + publicKey,
	}
	for _, cacheKey := range keys {
		if err := r.cache.Del(ctx, cacheKey).Err(); err != nil {
			slog.WarnContext(ctx, "failed to invalidate project cache", slogx.Error(err), slog.String("cacheKey", cacheKey))
		}
	}
}

func (r *Repo) GetProjectAndCustomerByPublicApiKey(ctx context.Context, publicApiKey string) (dbread.GetProjectAndCustomerByPublicApiKeyRow, error) {
	cacheKey := publicKeyCachePrefix + publicApiKey

	data, err := r.cache.Get(ctx, cacheKey).Bytes()
	if err != nil && !errors.Is(err, goredis.Nil) {
		slog.WarnContext(ctx, "failed to get project by public api key from cache", slogx.Error(err))
	} else if err == nil {
		var row dbread.GetProjectAndCustomerByPublicApiKeyRow
		if err := json.Unmarshal(data, &row); err != nil {
			slog.WarnContext(ctx, "failed to unmarshal cached project by public api key, deleting corrupt entry", slogx.Error(err))
			r.cache.Del(ctx, cacheKey)
		} else {
			return row, nil
		}
	}

	row, err := r.queries.GetProjectAndCustomerByPublicApiKey(ctx, publicApiKey)
	if err != nil {
		return dbread.GetProjectAndCustomerByPublicApiKeyRow{}, err
	}

	data, err = json.Marshal(row)
	if err != nil {
		slog.WarnContext(ctx, "failed to marshal project by public api key for caching", slogx.Error(err))
	} else if err := r.cache.Set(ctx, cacheKey, data, apiKeyCacheTTL).Err(); err != nil {
		slog.WarnContext(ctx, "failed to cache project by public api key", slogx.Error(err))
	}

	return row, nil
}
