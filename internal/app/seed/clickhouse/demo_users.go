package seed

import "time"

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
	out := make([]DemoUser, n)
	for i := range out {
		u := demoUserProfile(i)
		out[i] = DemoUser{
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
	return out
}
