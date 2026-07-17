package seed

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pug-sh/pug/internal/slogx"
)

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// DistinctIDPool is the number of distinct human users the generator emits
// events for. Exported as the single source of truth for the demo population
// size: the postgres profile seeder seeds exactly this many profiles, so
// seeding more would create profiles for distinct ids that never produce
// events and seeding fewer would leave emitted users profile-less.
const DistinctIDPool = 10_000

const (
	botIDPool      = 300
	memberShare    = 0.3 // members: signed-in, pushable, higher purchase intent
	botSessionRate = 0.025

	// userSeed keys each user's deterministic attribute stream. Every factory
	// instance (backfill run, live worker, restarts) derives identical users
	// from it, so user-00042 keeps the same city, devices, join date, and
	// engagement level everywhere.
	userSeed = 0xD06F00D

	// Lifecycle: joins spread uniformly over [userJoinEpoch,
	// userJoinEpoch+userJoinSpan). Users whose join falls inside the seeded
	// window produce signup cohorts; users joining after "now" surface later
	// as genuinely new users while the live worker runs.
	userJoinSpan = 3 * 365 * 24 * time.Hour

	// loyalUserShare of users stay for years; the rest churn with an
	// exponentially-distributed lifetime, which is what makes retention
	// cohorts decay instead of staying flat.
	loyalUserShare     = 0.40
	loyalLifetime      = 10 * 365 * 24 * time.Hour
	casualLifetimeMean = 45 * 24 * time.Hour

	// A session starting within firstSessionWindow of the user's join is
	// forced onto a signup/app-install journey, so signup events line up
	// with the user's first appearance.
	firstSessionWindow = 36 * time.Hour

	// userPickGranularity converts the continuous activity weight into
	// pick-table entries.
	userPickGranularity = 8
)

var userJoinEpoch = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

type userProfile struct {
	id       string
	member   bool
	geo      geoEntry
	devices  []deviceProfile // 1-3 stable devices per user
	join     time.Time       // first ever session (signup cohort)
	churn    time.Time       // goes quiet after this; loyal users effectively never
	activity float64         // relative session frequency (log-normal: whales vs one-timers)
}

// ---------------------------------------------------------------------------
// Events & sessions
// ---------------------------------------------------------------------------

type event struct {
	eventID          string
	distinctID       string
	sessionID        string
	kind             string
	occurTime        time.Time
	autoProperties   map[string]any
	customProperties map[string]any
}

// clampOccurTime caps t at max so seeded events never land in the future.
func clampOccurTime(t, max time.Time) time.Time {
	if t.After(max) {
		return max
	}
	return t
}

// sessionWindow tracks a single session's time range and platform for overlap detection.
type sessionWindow struct {
	start    time.Time
	end      time.Time
	platform string
}

// userSessionTracker records assigned session windows per user so overlapping
// same-platform sessions can be detected and moved to another device.
type userSessionTracker struct {
	sessions map[string][]sessionWindow
}

func newUserSessionTracker() *userSessionTracker {
	return &userSessionTracker{sessions: make(map[string][]sessionWindow)}
}

func (t *userSessionTracker) overlappingPlatforms(distinctID string, start, end time.Time) map[string]bool {
	out := map[string]bool{}
	for _, w := range t.sessions[distinctID] {
		if start.Before(w.end) && end.After(w.start) {
			out[w.platform] = true
		}
	}
	return out
}

func (t *userSessionTracker) register(distinctID, platform string, start, end time.Time) {
	t.sessions[distinctID] = append(t.sessions[distinctID], sessionWindow{start: start, end: end, platform: platform})
}

// pastOrder is a purchase remembered for later refund references.
type pastOrder struct {
	id     string
	amount float64
	t      time.Time
}

// userMemory carries cross-session state so later sessions can reference
// earlier ones: push-recovery reopens the cart that was actually abandoned,
// refunds cite real order ids and amounts, and trial conversions reuse the
// trial that was started. Entries are only consumed when they predate the
// current session, so out-of-order batch generation never violates
// causality; bounded per user so the live worker's memory stays flat.
type userMemory struct {
	abandonedCart  []cartLine
	abandonedAt    time.Time
	trialID        string
	trialStartedAt time.Time
	orders         []pastOrder // most recent last, capped

	// signedUp caps the forced signup/app-install journey at one per user.
	// The forcing is time-windowed (any session within firstSessionWindow of
	// join), so without this flag a high-activity or recently-joined user
	// emits several signup events. Set the first time a signup journey is
	// built and read by journeyFor to suppress later forces. Because the
	// backfill generates sessions out of order, "first" means first-generated,
	// not strictly-earliest — but every forced signup still lands inside the
	// join window, so the single retained signup is always near cohort entry.
	signedUp bool
}

const maxRememberedOrders = 5

func (m *userMemory) rememberOrder(o pastOrder) {
	m.orders = append(m.orders, o)
	if len(m.orders) > maxRememberedOrders {
		m.orders = m.orders[len(m.orders)-maxRememberedOrders:]
	}
}

// rememberAbandonedCart records a cart left un-purchased so a later session can
// resume it. Mirrors the read-side takeAbandonedCartBefore so every mutation of
// the causality-sensitive memory fields goes through the type rather than
// caller discipline.
func (m *userMemory) rememberAbandonedCart(cart []cartLine, at time.Time) {
	m.abandonedCart = cart
	m.abandonedAt = at
}

// rememberTrial records a started trial so a later session can convert it.
func (m *userMemory) rememberTrial(id string, at time.Time) {
	m.trialID = id
	m.trialStartedAt = at
}

// takeOrderBefore pops the oldest remembered order that predates t.
func (m *userMemory) takeOrderBefore(t time.Time) (pastOrder, bool) {
	for i, o := range m.orders {
		if o.t.Before(t) {
			m.orders = append(m.orders[:i], m.orders[i+1:]...)
			return o, true
		}
	}
	return pastOrder{}, false
}

// takeAbandonedCartBefore pops the remembered abandoned cart if it predates t.
func (m *userMemory) takeAbandonedCartBefore(t time.Time) ([]cartLine, bool) {
	if len(m.abandonedCart) == 0 || !m.abandonedAt.Before(t) {
		return nil, false
	}
	cart := m.abandonedCart
	m.abandonedCart = nil
	return cart, true
}

// takeTrialBefore pops the remembered trial id if it predates t.
func (m *userMemory) takeTrialBefore(t time.Time) (string, bool) {
	if m.trialID == "" || !m.trialStartedAt.Before(t) {
		return "", false
	}
	id := m.trialID
	m.trialID = ""
	return id, true
}

// sessionFactory generates sessions on demand. User attributes are derived
// from a per-user deterministic stream (see userSeed) so they are identical
// in every factory instance.
type sessionFactory struct {
	users    []userProfile
	userPick []int32 // user indices repeated ∝ activity, for O(1) weighted picks
	memories map[string]*userMemory
	tracker  *userSessionTracker
	locs     map[string]*time.Location
}

func newSessionFactory() *sessionFactory {
	f := &sessionFactory{
		memories: make(map[string]*userMemory),
		tracker:  newUserSessionTracker(),
		locs:     make(map[string]*time.Location),
	}

	f.users = make([]userProfile, DistinctIDPool)
	for i := range f.users {
		f.users[i] = demoUserProfile(i)
	}

	for i, u := range f.users {
		for range int(u.activity*userPickGranularity) + 1 {
			f.userPick = append(f.userPick, int32(i))
		}
	}
	return f
}

// demoUserProfile derives user i's stable attributes from the deterministic
// per-user stream. Surfaced via DemoUserAt to the postgres profile seeder and
// the live demo worker so profile properties agree with event data.
func demoUserProfile(i int) userProfile {
	r := rand.New(rand.NewPCG(userSeed, uint64(i)))

	devices := []deviceProfile{deviceProfiles[weightedIndexR(r, deviceWeightsAll())]}
	for r.Float32() < 0.35 && len(devices) < 3 {
		d := deviceProfiles[weightedIndexR(r, deviceWeightsAll())]
		if d.platform != devices[0].platform {
			devices = append(devices, d)
		}
	}

	join := userJoinEpoch.Add(time.Duration(r.Float64() * float64(userJoinSpan)))
	lifetime := time.Duration(loyalLifetime)
	if r.Float64() >= loyalUserShare {
		lifetime = max(24*time.Hour, time.Duration(r.ExpFloat64()*float64(casualLifetimeMean)))
	}
	// Log-normal engagement: a few whales, a long tail of one-timers.
	activity := min(max(math.Exp(0.9*r.NormFloat64()), 0.25), 12)

	return userProfile{
		id:       fmt.Sprintf("user-%05d", i),
		member:   i%10 < int(memberShare*10),
		geo:      geoPool[weightedIndexR(r, geoWeightsAll())],
		devices:  devices,
		join:     join,
		churn:    join.Add(lifetime),
		activity: activity,
	}
}

var (
	cachedGeoWeights    []int
	cachedDeviceWeights []int
)

func geoWeightsAll() []int {
	if cachedGeoWeights == nil {
		for _, g := range geoPool {
			cachedGeoWeights = append(cachedGeoWeights, g.weight)
		}
	}
	return cachedGeoWeights
}

func deviceWeightsAll() []int {
	if cachedDeviceWeights == nil {
		for _, d := range deviceProfiles {
			cachedDeviceWeights = append(cachedDeviceWeights, d.weight)
		}
	}
	return cachedDeviceWeights
}

// memory returns (lazily creating) the cross-session memory for a user.
func (f *sessionFactory) memory(u *userProfile) *userMemory {
	m, ok := f.memories[u.id]
	if !ok {
		m = &userMemory{}
		f.memories[u.id] = m
	}
	return m
}

// pickActiveUser draws an activity-weighted user whose lifecycle overlaps
// [start, end] and returns the overlap. Acceptance is additionally weighted
// by the overlap's share of the window so a user active for three days
// doesn't compress a full user's worth of sessions into them.
func (f *sessionFactory) pickActiveUser(start, end time.Time) (*userProfile, time.Time, time.Time) {
	window := end.Sub(start)
	for range 64 {
		u := &f.users[f.userPick[rand.IntN(len(f.userPick))]]
		aStart, aEnd := u.join, u.churn
		if aStart.Before(start) {
			aStart = start
		}
		if aEnd.After(end) {
			aEnd = end
		}
		if !aEnd.After(aStart) {
			continue
		}
		if window > 0 && rand.Float64() > float64(aEnd.Sub(aStart))/float64(window) {
			continue
		}
		return u, aStart, aEnd
	}
	// Degenerate windows (e.g. far outside the join span): ignore lifecycle
	// so generation always makes progress.
	u := &f.users[f.userPick[rand.IntN(len(f.userPick))]]
	return u, start, end
}

// warnedMissingTZ dedupes the missing-timezone warning to one log per zone.
var warnedMissingTZ sync.Map

func (f *sessionFactory) location(name string) *time.Location {
	if loc, ok := f.locs[name]; ok {
		return loc
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		// Falling back to UTC silently would collapse every region's diurnal
		// curve onto the same hours (a slim/distroless image without tzdata is
		// the usual cause), so surface it once per zone.
		if _, warned := warnedMissingTZ.LoadOrStore(name, struct{}{}); !warned {
			slog.WarnContext(context.Background(), "timezone not found, defaulting to UTC (diurnal curve for this region will be wrong)",
				slog.String("timezone", name), slogx.Error(err))
		}
		loc = time.UTC
	}
	f.locs[name] = loc
	return loc
}

// Diurnal traffic shapes, applied in the user's local timezone. Desktop
// leans into work hours, mobile into evenings, and weekends have no commute
// ramp — late mornings are already busy.
var hourWeights = [24]float64{ // aggregate (used by the live worker's rate)
	0.45, 0.35, 0.25, 0.22, 0.22, 0.28, // 00-05
	0.40, 0.55, 0.70, 0.85, 0.95, 1.00, // 06-11
	1.05, 1.00, 0.95, 0.95, 1.00, 1.10, // 12-17
	1.25, 1.40, 1.50, 1.45, 1.20, 0.80, // 18-23
}

var desktopHourWeights = [24]float64{
	0.30, 0.22, 0.18, 0.15, 0.15, 0.20, // 00-05
	0.35, 0.55, 0.80, 1.00, 1.10, 1.15, // 06-11
	1.20, 1.15, 1.10, 1.05, 1.00, 0.95, // 12-17
	0.90, 0.95, 1.00, 0.90, 0.70, 0.45, // 18-23
}

var mobileHourWeights = [24]float64{
	0.55, 0.40, 0.28, 0.25, 0.25, 0.32, // 00-05
	0.50, 0.65, 0.75, 0.80, 0.85, 0.90, // 06-11
	1.00, 0.95, 0.90, 0.90, 1.00, 1.15, // 12-17
	1.35, 1.55, 1.65, 1.60, 1.35, 0.90, // 18-23
}

var weekendHourWeights = [24]float64{
	0.50, 0.38, 0.28, 0.22, 0.20, 0.22, // 00-05
	0.30, 0.45, 0.70, 0.95, 1.15, 1.25, // 06-11
	1.25, 1.20, 1.15, 1.10, 1.10, 1.15, // 12-17
	1.25, 1.40, 1.50, 1.40, 1.15, 0.75, // 18-23
}

// Pronounced weekend lift so weekly humps are clearly visible on a
// day-granularity trends chart (Saturday ≈ 1.7x Tuesday).
var dayWeights = [7]float64{1.35, 0.85, 0.85, 0.90, 0.95, 1.10, 1.45} // Sun..Sat

// startTimeWeight combines diurnal/weekly curves with episode multipliers.
// Long-term growth is not synthesized here — it emerges from loyal users
// accumulating as join dates spread across the window. Max possible value:
// Sat/Sun force weekendHourWeights (peak 1.50) rather than the mobile table
// (1.65), so the weekend ceiling dominates — 1.50 (weekend hour peak) * 1.45
// (Saturday) * 1.45 (promo) ≈ 3.15; 3.5 is a safe over-estimate for the
// rejection sampler (an over-estimated denominator only costs sampling
// efficiency, never correctness).
const maxStartWeight = 3.5

func (f *sessionFactory) startTimeWeight(t time.Time, geo geoEntry, mobile bool) float64 {
	local := t.In(f.location(geo.timezone))
	hours := &desktopHourWeights
	if wd := local.Weekday(); wd == time.Saturday || wd == time.Sunday {
		hours = &weekendHourWeights
	} else if mobile {
		hours = &mobileHourWeights
	}
	return hours[local.Hour()] * dayWeights[int(local.Weekday())] * episodeTrafficMult(t)
}

// session generates one coherent session anchored inside [start, end].
func (f *sessionFactory) session(start, end time.Time) []event {
	if rand.Float64() < botSessionRate {
		anchor := start
		if totalMs := end.Sub(start).Milliseconds(); totalMs > 0 {
			anchor = start.Add(time.Duration(rand.Int64N(totalMs)) * time.Millisecond)
		}
		return f.botSession(anchor, end)
	}

	u, activeStart, activeEnd := f.pickActiveUser(start, end)
	prof := u.devices[rand.IntN(len(u.devices))]

	sessionStart := f.sampleStart(u.geo, prof.mobile, activeStart, activeEnd)
	jd := f.journeyFor(u, prof, sessionStart)

	// If this user already has an overlapping session on this platform, try
	// another of their devices; if all platforms are busy, tolerate overlap
	// (rare with a 10k user pool).
	approxEnd := sessionStart.Add(time.Duration(len(jd.steps)) * 90 * time.Second)
	if occupied := f.tracker.overlappingPlatforms(u.id, sessionStart, approxEnd); occupied[prof.platform] {
		for _, d := range u.devices {
			if !occupied[d.platform] {
				prof = d
				jd = f.journeyFor(u, prof, sessionStart)
				break
			}
		}
	}

	sess := buildSession(u, prof, jd, sessionStart, end, f.memory(u))
	if len(sess) == 0 {
		return sess
	}
	f.tracker.register(u.id, prof.platform, sess[0].occurTime, sess[len(sess)-1].occurTime)
	return sess
}

// sampleStart rejection-samples a session start so traffic follows the
// diurnal/weekly curve in the user's local timezone.
func (f *sessionFactory) sampleStart(geo geoEntry, mobile bool, start, end time.Time) time.Time {
	totalMs := end.Sub(start).Milliseconds()
	if totalMs <= 0 {
		return start
	}
	var candidate time.Time
	for range 16 {
		candidate = start.Add(time.Duration(rand.Int64N(totalMs)) * time.Millisecond)
		if rand.Float64() < f.startTimeWeight(candidate, geo, mobile)/maxStartWeight {
			return candidate
		}
	}
	return candidate
}

// journeyFor selects a journey. Forced selections, in priority order: the
// signup/install journey on a user's first session (so signups line up with
// cohort entry), purchase journeys during promo episodes, push journeys on
// blast days, and crash/error journeys right after an app release.
func (f *sessionFactory) journeyFor(u *userProfile, prof deviceProfile, sessionStart time.Time) journeyDef {
	app := isApp(prof.platform)

	// Force the signup/install journey on a user's first in-window session, but
	// only once: buildSession sets mem.signedUp when it emits the signup, so
	// later sessions inside the window fall through to the normal mix instead of
	// re-firing signup (which would inflate acquisition counts and put the user
	// in multiple day-0 cohorts).
	if sessionStart.Sub(u.join) < firstSessionWindow && !f.memory(u).signedUp {
		if app {
			return appInstallJourney
		}
		return webSignupJourney
	}

	if _, promo := activePromo(sessionStart); promo && rand.Float64() < promoPurchaseBoost {
		if app {
			return journeyByName(appJourneys, "app-purchase")
		}
		if rand.IntN(2) == 0 {
			return journeyByName(webJourneys, "purchase-multi-coupon")
		}
		return journeyByName(webJourneys, "purchase")
	}

	if app && u.member && pushBlastActive(sessionStart) && rand.Float64() < pushBlastJourneyShare {
		blast := []string{"push-recovery", "push-browse", "push-browse", "push-dismissed"}
		return journeyByName(appJourneys, blast[rand.IntN(len(blast))])
	}

	if app && recentAppRelease(sessionStart) && rand.Float64() < postReleaseCrashBoost {
		return journeyByName(appJourneys, "app-crash")
	}

	return pickJourney(prof.platform, u.member)
}

// isApp reports whether a platform is one of the native mobile apps (as opposed
// to web), which run a different journey mix.
func isApp(platform string) bool {
	return platform == "ios" || platform == "android"
}

func pickJourney(platform string, member bool) journeyDef {
	defs := webJourneys
	if isApp(platform) {
		defs = appJourneys
	}
	weights := make([]int, len(defs))
	for i, d := range defs {
		weights[i] = d.weight
	}
	for {
		d := defs[weightedIndex(weights)]
		if d.memberOnly && !member {
			continue // member-only journey drawn for a non-member; redraw
		}
		return d
	}
}

// ---------------------------------------------------------------------------
// Bot sessions
// ---------------------------------------------------------------------------

// botSession emits a short crawl (web only) starting at anchor: a few page
// views with a low $bot_score, $verified_bot for known crawlers, and no
// commerce activity.
func (f *sessionFactory) botSession(anchor, end time.Time) []event {
	bp := botProfiles[rand.IntN(len(botProfiles))]
	geo := geoPool[weightedIndex(geoWeightsAll())]
	distinctID := fmt.Sprintf("bot-%04d", rand.IntN(botIDPool))
	sessionID := uuid.New().String()

	props := map[string]any{
		"$platform":  "web",
		"$os":        "Linux",
		"$bot_score": 1 + rand.IntN(28),
		"$mobile":    false,
		"$continent": geo.continent,
		"$country":   geo.country,
		"$region":    geo.region,
		"$city":      geo.city,
		"$timezone":  geo.timezone,
		"$latitude":  round5(geo.latitude),
		"$longitude": round5(geo.longitude),
	}
	if bp.browser != "" {
		props["$browser"] = bp.browser
	}
	if bp.verified {
		props["$verified_bot"] = true
	}

	occur := anchor

	n := 2 + rand.IntN(4)
	sess := make([]event, 0, n)
	for i := range n {
		if i > 0 {
			occur = occur.Add(time.Duration(1+rand.Int64N(8)) * time.Second)
		}
		p := catalog[rand.IntN(len(catalog))]
		auto := copyProps(props)
		auto["$url"] = storeURL + "/products/" + p.slug
		auto["$pageTitle"] = p.name + " — Pug & Pals"
		applyAttribution(auto)
		sess = append(sess, event{
			eventID:          uuid.New().String(),
			distinctID:       distinctID,
			sessionID:        sessionID,
			kind:             "page_view",
			occurTime:        clampOccurTime(occur, end),
			autoProperties:   auto,
			customProperties: map[string]any{},
		})
	}
	return sess
}
