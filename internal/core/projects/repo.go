package projects

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
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

func (r *Repo) GetProjectByPrivateApiKey(ctx context.Context, privateApiKey string) (dbread.Project, error) {
	cacheKey := privateKeyCachePrefix + privateApiKey

	data, err := r.cache.Get(ctx, cacheKey).Bytes()
	if err != nil && !errors.Is(err, goredis.Nil) {
		slog.WarnContext(ctx, "failed to get project by private api key from cache", slogx.Error(err))
	} else if err == nil {
		var project dbread.Project
		if err := json.Unmarshal(data, &project); err != nil {
			slog.WarnContext(ctx, "failed to unmarshal cached project by private api key, deleting corrupt entry", slogx.Error(err))
			if err := r.cache.Del(ctx, cacheKey).Err(); err != nil {
				slog.WarnContext(ctx, "failed to delete corrupt cache entry", slogx.Error(err), slog.String("cache_key", cacheKey))
			}
		} else {
			return project, nil
		}
	}

	project, err := r.queries.GetProjectByPrivateApiKey(ctx, privateApiKey)
	if err != nil {
		// pgx.ErrNoRows is the expected "invalid API key" miss — caller translates to authn error.
		// Real DB failures (connection, timeout, etc.) log + record at source.
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "failed to query project by private api key", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
		return dbread.Project{}, err
	}

	data, err = json.Marshal(project)
	if err != nil {
		slog.WarnContext(ctx, "failed to marshal project by private api key for caching", slogx.Error(err))
	} else if err := r.cache.Set(ctx, cacheKey, data, apiKeyCacheTTL).Err(); err != nil {
		slog.WarnContext(ctx, "failed to cache project by private api key", slogx.Error(err))
	}

	return project, nil
}

func (r *Repo) InvalidateProjectKeys(ctx context.Context, privateKey, publicKey string) {
	keys := []string{
		privateKeyCachePrefix + privateKey,
		publicKeyCachePrefix + publicKey,
	}
	for _, cacheKey := range keys {
		if err := r.cache.Del(ctx, cacheKey).Err(); err != nil {
			slog.WarnContext(ctx, "failed to invalidate project cache", slogx.Error(err), slog.String("cache_key", cacheKey))
		}
	}
}

func (r *Repo) GetProjectByPublicApiKey(ctx context.Context, publicApiKey string) (dbread.Project, error) {
	cacheKey := publicKeyCachePrefix + publicApiKey

	data, err := r.cache.Get(ctx, cacheKey).Bytes()
	if err != nil && !errors.Is(err, goredis.Nil) {
		slog.WarnContext(ctx, "failed to get project by public api key from cache", slogx.Error(err))
	} else if err == nil {
		var project dbread.Project
		if err := json.Unmarshal(data, &project); err != nil {
			slog.WarnContext(ctx, "failed to unmarshal cached project by public api key, deleting corrupt entry", slogx.Error(err))
			if err := r.cache.Del(ctx, cacheKey).Err(); err != nil {
				slog.WarnContext(ctx, "failed to delete corrupt cache entry", slogx.Error(err), slog.String("cache_key", cacheKey))
			}
		} else {
			return project, nil
		}
	}

	project, err := r.queries.GetProjectByPublicApiKey(ctx, publicApiKey)
	if err != nil {
		// pgx.ErrNoRows is the expected "invalid API key" miss — caller translates to authn error.
		// Real DB failures (connection, timeout, etc.) log + record at source.
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "failed to query project by public api key", slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
		return dbread.Project{}, err
	}

	data, err = json.Marshal(project)
	if err != nil {
		slog.WarnContext(ctx, "failed to marshal project by public api key for caching", slogx.Error(err))
	} else if err := r.cache.Set(ctx, cacheKey, data, apiKeyCacheTTL).Err(); err != nil {
		slog.WarnContext(ctx, "failed to cache project by public api key", slogx.Error(err))
	}

	return project, nil
}
