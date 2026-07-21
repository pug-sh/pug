package cookieless

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// ErrCorruptSalt is returned when the stored salt for a day cannot be decoded or
// is the wrong length.
//
// It is separated from a transient fetch failure because the two need opposite
// operator responses. A Redis outage clears itself and the same events succeed
// on retry; a corrupt value is re-read and re-rejected for the life of the key,
// because SETNX only mints when GET reports the key absent — so nothing
// overwrites it. Only this package writes these keys, so a corrupt one implies
// an external writer or a saltLen change during a rolling deploy. Either needs a
// human, and folding it into the generic drop reason told operators to retry.
var ErrCorruptSalt = errors.New("cookieless: corrupt salt")

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

// saltTTLFor returns how long day D's salt may live: until the instant it leaves
// DayOf's accepted window (D+2 00:00 UTC), never longer.
//
// The TTL is the deletion mechanism the whole privacy guarantee rests on, so it
// is anchored to the DAY rather than to mint time. SETNX stamps expiry when the
// key is first written and never refreshes it, and a salt is minted lazily on
// the first event attributed to its day — which an offline-buffered flush (the
// case the two-day window exists to serve) can push to nearly D+2. A flat
// duration therefore floats with traffic: a 72h TTL kept a salt re-derivable
// until as late as D+5, three days after any code path could still use it, and
// left up to five salts coexisting rather than the two DayOf's doc claims.
//
// A non-positive or over-long result means the day is outside the accepted
// window, which makes this the boundary guard for saltForDay as well — the
// window and the salt's lifetime become the same fact, computed once.
func (r *Resolver) saltTTLFor(day Day) (time.Duration, error) {
	d, err := time.ParseInLocation(dayFormat, string(day), time.UTC)
	if err != nil {
		return 0, fmt.Errorf("cookieless: malformed day %q", day)
	}
	ttl := d.AddDate(0, 0, 2).Sub(r.now().UTC())
	if ttl <= 0 || ttl > saltMaxTTL {
		return 0, fmt.Errorf("cookieless: day %s is outside the accepted window", day)
	}
	return ttl, nil
}

// saltForDay returns the day's salt: in-process cache, then Redis, then a
// SETNX mint race that every pod can enter safely — the first writer wins and
// losers re-read the winner's value. The Redis TTL is the deletion mechanism
// that makes rotated-out hashes permanently unlinkable.
//
// The in-process cache is pruned for the same reason, but note the prune is
// LAZY: it runs inside cacheSalt, i.e. only on a miss. That is sufficient rather
// than incidental — a day enters the map only via cacheSalt, so the first event
// of any new day is necessarily a miss and necessarily prunes — but a pod that
// stops seeing cookieless traffic keeps its last two salts (64 bytes) in RAM
// until its next one. Unreachable for minting either way, since the caller
// rejects an out-of-window day before it gets here.
//
// The returned slice is the shared cache entry, not a copy. Treat it as
// read-only; the only consumer is hmac.New, which copies the key into its
// ipad/opad buffers and neither retains nor mutates it.
//
// Rejects a day outside the accepted window before touching Redis, so a
// malformed or stale day can never mint a key that outlives its usefulness.
func (r *Resolver) saltForDay(ctx context.Context, day Day) ([]byte, error) {
	ttl, err := r.saltTTLFor(day)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if s, ok := r.salts[day]; ok {
		r.mu.Unlock()
		saltCacheHitCounter.Add(ctx, 1)
		return s, nil
	}
	r.mu.Unlock()
	saltCacheMissCounter.Add(ctx, 1)

	key := saltKeyPrefix + string(day)
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
	set, err := r.rdb.SetNX(ctx, key, base64.StdEncoding.EncodeToString(fresh), ttl).Result()
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

func (r *Resolver) storeSalt(day Day, encoded string) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w for %s: not base64: %w", ErrCorruptSalt, day, err)
	}
	if len(salt) != saltLen {
		return nil, fmt.Errorf("%w for %s: got %d bytes, want %d", ErrCorruptSalt, day, len(salt), saltLen)
	}
	r.cacheSalt(day, salt)
	return salt, nil
}

func (r *Resolver) cacheSalt(day Day, salt []byte) {
	now := r.now().UTC()
	today, yesterday := Day(now.Format(dayFormat)), Day(now.AddDate(0, 0, -1).Format(dayFormat))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.salts[day] = salt
	for d := range r.salts {
		if d != today && d != yesterday {
			delete(r.salts, d)
		}
	}
}
