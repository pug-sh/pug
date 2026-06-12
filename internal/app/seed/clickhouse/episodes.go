package seed

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// Episodes are scripted, deterministic calendar events that give the demo
// data a narrative: promo weeks where traffic and purchases spike, push-blast
// days, and app releases followed by short error spikes. Everything is
// anchored to userJoinEpoch so the backfill, the live worker, and restarts
// all agree on the calendar, and any seeded window contains a few episodes.

const (
	// Each 6-week cycle contains one 5-day promo and one push-blast day.
	episodeCycle   = 42 * 24 * time.Hour
	promoStart     = 14 * 24 * time.Hour // day 14 of the cycle
	promoLen       = 5 * 24 * time.Hour
	pushBlastStart = 28 * 24 * time.Hour // day 28 of the cycle
	pushBlastLen   = 24 * time.Hour

	promoTrafficMult     = 1.45
	pushBlastTrafficMult = 1.15

	// During a promo: share of sessions arriving via campaigns, how many of
	// those carry the promo's own campaign name, and how often a session is
	// steered into a purchase journey.
	promoAcquisitionShare = 0.45
	promoCampaignShare    = 0.70
	promoPurchaseBoost    = 0.05

	pushBlastJourneyShare = 0.40
)

// promoCampaigns rotate per cycle so consecutive promos have different names
// in UTM breakdowns.
var promoCampaigns = []string{"summer-sale", "new-arrivals", "back-in-stock", "pug-club-launch", "treat-tuesday", "price-drop"}

func cycleAt(t time.Time) (idx int, offset time.Duration) {
	since := t.Sub(userJoinEpoch)
	idx = int(since / episodeCycle)
	offset = since % episodeCycle
	if offset < 0 { // before the epoch
		idx--
		offset += episodeCycle
	}
	return idx, offset
}

// activePromo reports whether a promo runs at t and which campaign it is.
func activePromo(t time.Time) (string, bool) {
	idx, offset := cycleAt(t)
	if offset < promoStart || offset >= promoStart+promoLen {
		return "", false
	}
	return promoCampaigns[((idx%len(promoCampaigns))+len(promoCampaigns))%len(promoCampaigns)], true
}

// pushBlastActive reports whether t falls on the cycle's push-blast day.
func pushBlastActive(t time.Time) bool {
	_, offset := cycleAt(t)
	return offset >= pushBlastStart && offset < pushBlastStart+pushBlastLen
}

// episodeTrafficMult scales overall traffic during episodes.
func episodeTrafficMult(t time.Time) float64 {
	if _, ok := activePromo(t); ok {
		return promoTrafficMult
	}
	if pushBlastActive(t) {
		return pushBlastTrafficMult
	}
	return 1
}

// ---------------------------------------------------------------------------
// App releases & version adoption
// ---------------------------------------------------------------------------

const (
	appReleaseInterval = 8 * 7 * 24 * time.Hour // a release every 8 weeks
	appReleaseCount    = 24                     // covers the join span and then some

	// Error spike window after a release.
	postReleaseGrace = 3 * 24 * time.Hour
	postReleaseCrashBoost = 0.08

	// Adoption shape: a new version ramps in over ~days and old versions
	// decay away over ~weeks.
	adoptionRampDays  = 5.0
	adoptionDecayDays = 70.0
)

// appReleases[i] is released at userJoinEpoch + i*appReleaseInterval.
var appReleases []string

func init() {
	major, minor, patch := 1, 0, 0
	for i := range appReleaseCount {
		appReleases = append(appReleases, fmt.Sprintf("%d.%d.%d", major, minor, patch))
		switch {
		case (i+1)%8 == 0:
			major, minor, patch = major+1, 0, 0
		case (i+1)%3 == 0:
			patch++
		default:
			minor, patch = minor+1, 0
		}
	}
}

func releaseTime(i int) time.Time {
	return userJoinEpoch.Add(time.Duration(i) * appReleaseInterval)
}

// releasedCount returns how many versions are out at t (at least 1).
func releasedCount(t time.Time) int {
	n := int(t.Sub(userJoinEpoch)/appReleaseInterval) + 1
	return min(max(n, 1), len(appReleases))
}

// latestAppVersionAt returns the newest released version at t.
func latestAppVersionAt(t time.Time) string {
	return appReleases[releasedCount(t)-1]
}

// appVersionAt picks a version weighted by an adoption curve: the newest
// release ramps in over ~adoptionRampDays while older versions decay with an
// ~adoptionDecayDays scale, so a "sessions by app_version" breakdown shows
// realistic adoption waves.
func appVersionAt(t time.Time) string {
	n := releasedCount(t)
	weights := make([]float64, n)
	var total float64
	for i := range n {
		ageDays := t.Sub(releaseTime(i)).Hours() / 24
		w := (1 - math.Exp(-ageDays/adoptionRampDays)) * math.Exp(-ageDays/adoptionDecayDays)
		weights[i] = w
		total += w
	}
	if total <= 0 {
		return appReleases[n-1]
	}
	r := rand.Float64() * total
	for i, w := range weights {
		r -= w
		if r < 0 {
			return appReleases[i]
		}
	}
	return appReleases[n-1]
}

// recentAppRelease reports whether t is within the error-spike grace period
// after a release.
func recentAppRelease(t time.Time) bool {
	since := t.Sub(releaseTime(releasedCount(t) - 1))
	return since >= 0 && since < postReleaseGrace
}
