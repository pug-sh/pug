// Package cookieless derives server-side visitor identity for consent-rejecting
// (GDPR/DPDP) clients that store nothing on the device. The identity is a
// daily-rotating keyed hash of transport facts the request already carries
// (project, IP, User-Agent); the day's salt lives in Redis and is deleted by
// TTL, after which stored hashes are unlinkable to any IP/UA by anyone —
// that deletion is the privacy guarantee. IDs carry the reserved IDPrefix so
// every downstream system (ClickHouse MVs, insights builders, protovalidate
// patterns) can classify them from the value alone.
package cookieless

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// IDPrefix marks every server-minted cookieless distinct_id. It partitions
	// the anonymous id space next to the SDK's "anon-" convention and is
	// load-bearing in ClickHouse migration 011 and the insights builders —
	// pinned by TestMigration011CookielessPrefixMatchesGo.
	IDPrefix = "cookieless-"

	dayFormat         = "20060102"
	saltKeyPrefix     = "cookieless:salt:"
	sessKeyPrefix     = "cookieless:sess:"
	saltLen           = 32
	saltMaxTTL        = 48 * time.Hour
	sessionTTL        = 24 * time.Hour
	sessionInactivity = 30 * time.Minute
)

// Day is a UTC calendar day in yyyymmdd form — the unit the salt rotates on.
//
// It is a named type rather than a bare string because this package threads
// several same-shaped strings side by side (day, projectID, distinctID, ip, ua),
// and transposing any two of them compiles, runs, and returns a
// confident-looking id. That is not hypothetical: swapping day and projectID at
// the DistinctID call site keyed the salt by project — one salt per project,
// minted once, never rotating, silently destroying the daily-rotation guarantee
// this package exists to provide — and the entire suite stayed green. A test can
// only pin the call sites someone thought to test; the type pins all of them.
type Day string

// Resolver derives cookieless identity. Safe for concurrent use.
type Resolver struct {
	rdb redis.Cmdable
	now func() time.Time

	mu    sync.Mutex
	salts map[Day][]byte // pruned to the accepted window
}

func New(rdb redis.Cmdable) *Resolver {
	return &Resolver{rdb: rdb, now: time.Now, salts: make(map[Day][]byte)}
}

// DayOf returns the UTC calendar day an occur_time hashes under and whether
// that day is inside the accepted window: today or yesterday, UTC — wide
// enough for SDK offline buffering, narrow enough that at most two salts can
// key new ids. How long a salt SURVIVES is saltTTLFor, not this window; the two
// agree only because that TTL expires exactly when the day leaves this window. A client clock skewed past midnight lands on "tomorrow" and is
// dropped by the caller (visible via its drop counter); accepted for
// salt-lifecycle simplicity.
func (r *Resolver) DayOf(occur time.Time) (Day, bool) {
	day := Day(occur.UTC().Format(dayFormat))
	now := r.now().UTC()
	ok := day == Day(now.Format(dayFormat)) || day == Day(now.AddDate(0, 0, -1).Format(dayFormat))
	return day, ok
}

// DistinctID derives the ephemeral id for one visitor-day:
//
//	IDPrefix + base64url-unpadded(HMAC-SHA256(salt_day, project ‖ 0x00 ‖ ip ‖ 0x00 ‖ ua)[:16])
//
// 16 bytes under RawURLEncoding is 22 characters (padded would be 24). The
// framing is injective because geo.ClientIP validates ip, so no field before the
// last can contain the 0x00 separator.
//
// IP and UA are hash inputs only — never stored, never returned. The only
// error is salt unavailability (Redis unreachable with a cold cache); the
// caller drops those events rather than fabricating identity.
func (r *Resolver) DistinctID(ctx context.Context, day Day, projectID, ip, ua string) (string, error) {
	salt, err := r.saltForDay(ctx, day)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(projectID))
	mac.Write([]byte{0})
	mac.Write([]byte(ip))
	mac.Write([]byte{0})
	mac.Write([]byte(ua))
	return IDPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16]), nil
}
