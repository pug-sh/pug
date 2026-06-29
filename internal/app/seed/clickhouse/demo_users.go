package seed

import (
	"fmt"
	"time"
)

// DemoUser exposes a demo user's stable, deterministically-derived
// attributes to other seeders — the postgres profile seeder uses this so
// profile properties (home city, membership, signup date) agree with the
// user's event data.
type DemoUser struct {
	ID       string
	Member   bool
	City     string
	Region   string
	Country  string
	Timezone string
	Locale   string
	Join     time.Time
}

// DemoUsers returns the first n demo users. Attributes match what the event
// generator produces for the same distinct ids, in every factory instance.
func DemoUsers(n int) []DemoUser {
	if n > DistinctIDPool {
		// Over-asking would fabricate profiles for distinct ids the event
		// generator never emits — fail loudly rather than silently drift.
		panic(fmt.Sprintf("seed: DemoUsers(%d) exceeds the user pool of %d", n, DistinctIDPool))
	}
	out := make([]DemoUser, n)
	for i := range out {
		out[i] = demoUserAt(i)
	}
	return out
}

// DemoUserAt returns the single demo user at index i. Used by the live demo
// worker to build a just-joined user's profile without materializing the whole
// pool. Panics on an out-of-range index (the caller derives i from a
// generator-emitted user-%05d id, so a bad index is a bug, not bad input).
func DemoUserAt(i int) DemoUser {
	if i < 0 || i >= DistinctIDPool {
		panic(fmt.Sprintf("seed: DemoUserAt(%d) out of range [0,%d)", i, DistinctIDPool))
	}
	return demoUserAt(i)
}

func demoUserAt(i int) DemoUser {
	u := demoUserProfile(i)
	return DemoUser{
		ID:       u.id,
		Member:   u.member,
		City:     u.geo.city,
		Region:   u.geo.region,
		Country:  u.geo.country,
		Timezone: u.geo.timezone,
		Locale:   u.geo.locale,
		Join:     u.join,
	}
}
