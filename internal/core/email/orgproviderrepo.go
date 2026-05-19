package email

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
	orgEmailProviderCachePrefix = "email:org_provider:"
	orgEmailProviderCacheTTL    = 5 * time.Minute
)

// CachedProviderEntry holds the cacheable representation of an org's email
// provider row. SecretCiphertext is the encrypted blob; plaintext NEVER lands
// in Dragonfly. Present=false means "no provider configured for this org" and
// is negative-cached to keep the operator-default fallback path cheap.
type CachedProviderEntry struct {
	Present          bool   `json:"present"`
	Kind             string `json:"kind,omitempty"`
	FromAddress      string `json:"from_address,omitempty"`
	ReplyTo          string `json:"reply_to,omitempty"`
	SecretCiphertext []byte `json:"secret_ciphertext,omitempty"`
}

type OrgProviderRepo struct {
	queries *dbread.Queries
	cache   *goredis.Client
}

func NewOrgProviderRepo(queries *dbread.Queries, cache *goredis.Client) *OrgProviderRepo {
	return &OrgProviderRepo{queries: queries, cache: cache}
}

func (r *OrgProviderRepo) Get(ctx context.Context, orgID string) (CachedProviderEntry, error) {
	cacheKey := orgEmailProviderCachePrefix + orgID

	if r.cache != nil {
		data, err := r.cache.Get(ctx, cacheKey).Bytes()
		switch {
		case err == nil:
			var entry CachedProviderEntry
			if jerr := json.Unmarshal(data, &entry); jerr == nil {
				return entry, nil
			}
			slog.WarnContext(ctx, "corrupt org email provider cache entry; deleting",
				slog.String("cache_key", cacheKey))
			if derr := r.cache.Del(ctx, cacheKey).Err(); derr != nil {
				slog.WarnContext(ctx, "failed to delete corrupt cache entry",
					slogx.Error(derr), slog.String("cache_key", cacheKey))
			}
		case errors.Is(err, goredis.Nil):
		default:
			slog.WarnContext(ctx, "failed to read org email provider cache",
				slogx.Error(err), slog.String("cache_key", cacheKey))
		}
	}

	row, err := r.queries.GetOrgEmailProvider(ctx, orgID)
	entry := CachedProviderEntry{Present: false}
	switch {
	case err == nil:
		entry = CachedProviderEntry{
			Present:          true,
			Kind:             row.Kind,
			FromAddress:      row.FromAddress,
			ReplyTo:          row.ReplyTo.String,
			SecretCiphertext: row.SecretCiphertext,
		}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		slog.ErrorContext(ctx, "failed to fetch org email provider", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return CachedProviderEntry{}, err
	}

	if r.cache != nil {
		if data, mErr := json.Marshal(entry); mErr == nil {
			if sErr := r.cache.Set(ctx, cacheKey, data, orgEmailProviderCacheTTL).Err(); sErr != nil {
				slog.WarnContext(ctx, "failed to cache org email provider",
					slogx.Error(sErr), slog.String("cache_key", cacheKey))
			}
		}
	}
	return entry, nil
}

// Invalidate deletes the cached entry for orgID. Callers must surface the
// error: a silent failure would let an admin's freshly-updated provider be
// shadowed by a stale negative-cache entry for the rest of its TTL.
func (r *OrgProviderRepo) Invalidate(ctx context.Context, orgID string) error {
	if r.cache == nil {
		return nil
	}
	cacheKey := orgEmailProviderCachePrefix + orgID
	if err := r.cache.Del(ctx, cacheKey).Err(); err != nil {
		slog.ErrorContext(ctx, "failed to invalidate email provider cache",
			slogx.Error(err), slog.String("cache_key", cacheKey))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}
