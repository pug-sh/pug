package cookieless

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	saltCacheHitCounter  metric.Int64Counter
	saltCacheMissCounter metric.Int64Counter
)

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/cookieless")
	saltCacheHitCounter, _ = meter.Int64Counter(
		"cookieless.salt_cache_hit_total",
		metric.WithDescription("Daily cookieless salt served from the in-process cache."),
	)
	saltCacheMissCounter, _ = meter.Int64Counter(
		"cookieless.salt_cache_miss_total",
		metric.WithDescription("Daily cookieless salt fetched (or minted) from Redis. Expect ~2/day/pod; a sustained elevated rate means the local cache is not retaining salts."),
	)
}

// saltForDay returns the day's salt: in-process cache, then Redis, then a
// SETNX mint race that every pod can enter safely — the first writer wins and
// losers re-read the winner's value. The Redis TTL is the deletion mechanism
// that makes rotated-out hashes permanently unlinkable; the in-process cache
// is pruned to the accepted window for the same reason (old salts must not
// linger in RAM).
func (r *Resolver) saltForDay(ctx context.Context, day string) ([]byte, error) {
	r.mu.Lock()
	if s, ok := r.salts[day]; ok {
		r.mu.Unlock()
		saltCacheHitCounter.Add(ctx, 1)
		return s, nil
	}
	r.mu.Unlock()
	saltCacheMissCounter.Add(ctx, 1)

	key := saltKeyPrefix + day
	val, err := r.rdb.Get(ctx, key).Result()
	switch {
	case err == nil:
		return r.storeSalt(day, val)
	case !errors.Is(err, redis.Nil):
		return nil, fmt.Errorf("cookieless: fetch salt: %w", err)
	}

	fresh := make([]byte, saltLen)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("cookieless: mint salt: %w", err)
	}
	set, err := r.rdb.SetNX(ctx, key, base64.StdEncoding.EncodeToString(fresh), saltTTL).Result()
	if err != nil {
		return nil, fmt.Errorf("cookieless: mint salt: %w", err)
	}
	if set {
		r.cacheSalt(day, fresh)
		return fresh, nil
	}
	// Lost the mint race — another pod's salt is authoritative.
	val, err = r.rdb.Get(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("cookieless: re-read raced salt: %w", err)
	}
	return r.storeSalt(day, val)
}

func (r *Resolver) storeSalt(day, encoded string) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(salt) != saltLen {
		return nil, fmt.Errorf("cookieless: corrupt salt for %s", day)
	}
	r.cacheSalt(day, salt)
	return salt, nil
}

func (r *Resolver) cacheSalt(day string, salt []byte) {
	now := r.now().UTC()
	today, yesterday := now.Format(dayFormat), now.AddDate(0, 0, -1).Format(dayFormat)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.salts[day] = salt
	for d := range r.salts {
		if d != today && d != yesterday {
			delete(r.salts, d)
		}
	}
}
