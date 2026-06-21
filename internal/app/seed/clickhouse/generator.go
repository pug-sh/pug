package seed

import (
	"maps"
	"math"
	"math/rand/v2"

	"github.com/google/uuid"
)

// The generator simulates traffic for "Pug & Pals" — a fictional
// direct-to-consumer dog supply store (food, treats, toys, grooming, gear,
// home) with a web storefront, iOS/Android apps, and a paid "Pug Club"
// membership.
//
// Realism rules the generator enforces:
//   - Users are stable and deterministic: each distinct_id derives its home
//     city, device set, member status, join date, churn date, and engagement
//     level from a fixed per-user seed, so every factory instance (backfill,
//     live worker, restarts) agrees on who each user is.
//   - Engagement follows a power law (a few whales, a long tail of
//     one-timers), users join over time and many churn — signup events fire
//     on a user's first session, so signup cohorts line up with retention
//     cohorts and the retention grid decays like a real product's.
//   - Sessions are stateful: a checkout funnel carries one cart through
//     product_viewed → add_to_cart → checkout_started → purchase with
//     consistent product ids and amounts.
//   - Event names and property shapes follow the well-known event catalog
//     in proto/common/events/v1 (purchase, screen_view, signin, ...).
//   - Device/OS/browser combinations are coherent (Safari only on Apple
//     platforms, $device absent on Windows/Linux desktops like a real UA
//     parse, $mobile true for phones and apps).
//   - Session start times follow a diurnal curve in the user's local
//     timezone, a weekly cycle, and a slow upward traffic trend.
//   - A small share of traffic is crawler/bot sessions carrying low
//     $bot_score and $verified_bot, so bot filters have data.

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func weightedIndex(weights []int) int {
	return weightedIndexN(rand.IntN, weights)
}

// weightedIndexR is weightedIndex driven by a caller-owned source, used for
// deterministic per-user attribute streams.
func weightedIndexR(r *rand.Rand, weights []int) int {
	return weightedIndexN(r.IntN, weights)
}

func weightedIndexN(intN func(int) int, weights []int) int {
	total := 0
	for _, w := range weights {
		total += w
	}
	if total <= 0 {
		// All-zero or empty weights have no valid weighted pick; return the
		// first index rather than panicking in intN(0). Every caller passes a
		// non-empty, positive-weight table today — this only guards a future
		// degenerate one.
		return 0
	}
	n := intN(total)
	for i, w := range weights {
		n -= w
		if n < 0 {
			return i
		}
	}
	return len(weights) - 1
}

func shortID() string {
	return uuid.New().String()[:8]
}

func round2(x float64) float64 {
	return math.Round(x*100) / 100
}

func round5(x float64) float64 {
	return math.Round(x*100000) / 100000
}

func copyProps(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src)+4)
	maps.Copy(dst, src)
	return dst
}
