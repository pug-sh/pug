package seed

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClampOccurTime(t *testing.T) {
	max := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	past := max.Add(-time.Hour)
	future := max.Add(time.Hour)

	if got := clampOccurTime(past, max); !got.Equal(past) {
		t.Fatalf("past time changed: got %v want %v", got, past)
	}
	if got := clampOccurTime(future, max); !got.Equal(max) {
		t.Fatalf("future time not clamped: got %v want %v", got, max)
	}
}

func TestSessionNoFutureEvents(t *testing.T) {
	end := time.Now()
	start := end.AddDate(0, -4, 0)
	factory := newSessionFactory()

	for range 1000 {
		sess := factory.session(start, end)
		for i, e := range sess {
			if e.occurTime.After(end) {
				t.Fatalf("session[%d]: occur_time %v is after end %v", i, e.occurTime, end)
			}
		}
	}
}

// Every human session must carry the core auto-properties on every event,
// with coherent platform/browser/device combinations: browser implies web or
// mobile web, native app events carry $device and $mobile=true, and all
// events have geo + bot score.
func TestSessionAutoPropertyCoverage(t *testing.T) {
	end := time.Now()
	start := end.AddDate(0, -1, 0)
	factory := newSessionFactory()

	for range 500 {
		sess := factory.session(start, end)
		for _, e := range sess {
			a := e.autoProperties
			for _, key := range []string{"$platform", "$os", "$country", "$city", "$latitude", "$longitude", "$bot_score", "$mobile"} {
				if _, ok := a[key]; !ok {
					t.Fatalf("event %s missing %s: %v", e.kind, key, a)
				}
			}
			platform := a["$platform"].(string)
			if platform == "ios" || platform == "android" {
				if _, ok := a["$device"]; !ok {
					t.Fatalf("app event %s missing $device", e.kind)
				}
				if a["$mobile"] != true {
					t.Fatalf("app event %s has $mobile=%v, want true", e.kind, a["$mobile"])
				}
				if _, ok := a["$app_version"]; !ok {
					t.Fatalf("app event %s missing $app_version", e.kind)
				}
			}
			if platform == "web" {
				if score, ok := a["$bot_score"].(int); ok && score >= 30 {
					if _, hasBrowser := a["$browser"]; !hasBrowser {
						t.Fatalf("human web event %s missing $browser", e.kind)
					}
					if _, ok := a["$url"]; !ok {
						t.Fatalf("web event %s missing $url", e.kind)
					}
				}
			}
		}
	}
}

// User attributes must be identical across factory instances: the backfill,
// the live worker, and any restart must agree on who each user is, or
// retention and user-flow break across the seam.
func TestUserPoolDeterministic(t *testing.T) {
	a := newSessionFactory()
	b := newSessionFactory()

	for i := range a.users {
		ua, ub := a.users[i], b.users[i]
		if ua.geo.city != ub.geo.city ||
			!ua.join.Equal(ub.join) ||
			!ua.churn.Equal(ub.churn) ||
			ua.activity != ub.activity ||
			len(ua.devices) != len(ub.devices) {
			t.Fatalf("user %d differs between factory instances:\n%+v\n%+v", i, ua, ub)
		}
		for j := range ua.devices {
			if ua.devices[j].platform != ub.devices[j].platform || ua.devices[j].device != ub.devices[j].device {
				t.Fatalf("user %d device %d differs: %+v vs %+v", i, j, ua.devices[j], ub.devices[j])
			}
		}
	}
}

// DemoUsers (the postgres profile seeder's view of a user) must agree with the
// event generator's view, or seeded profiles drift from the events they belong
// to — same city, membership, and join date for the same distinct id.
func TestDemoUsersMatchFactory(t *testing.T) {
	f := newSessionFactory()
	users := DemoUsers(len(f.users))
	if len(users) != len(f.users) {
		t.Fatalf("DemoUsers returned %d users, factory has %d", len(users), len(f.users))
	}

	for _, i := range []int{0, 1, 42, 100, 999, 5000, len(f.users) - 1} {
		want, got := f.users[i], users[i]
		switch {
		case got.ID != want.id:
			t.Errorf("user %d: ID = %q, want %q", i, got.ID, want.id)
		case got.City != want.geo.city:
			t.Errorf("user %d: City = %q, want %q", i, got.City, want.geo.city)
		case got.Member != want.member:
			t.Errorf("user %d: Member = %v, want %v", i, got.Member, want.member)
		case !got.Join.Equal(want.join):
			t.Errorf("user %d: Join = %v, want %v", i, got.Join, want.join)
		case got.Region != want.geo.region:
			t.Errorf("user %d: Region = %q, want %q", i, got.Region, want.geo.region)
		case got.Country != want.geo.country:
			t.Errorf("user %d: Country = %q, want %q", i, got.Country, want.geo.country)
		case got.Timezone != want.geo.timezone:
			t.Errorf("user %d: Timezone = %q, want %q", i, got.Timezone, want.geo.timezone)
		case got.Locale != want.geo.locale:
			t.Errorf("user %d: Locale = %q, want %q", i, got.Locale, want.geo.locale)
		}
	}
}

// Sessions must respect user lifecycle: no events before a user's join, and
// none meaningfully after their churn (a session may overrun churn by its
// own duration).
func TestSessionsRespectUserLifecycle(t *testing.T) {
	f := newSessionFactory()
	end := time.Now()
	start := end.AddDate(0, -4, 0)

	byID := make(map[string]*userProfile, len(f.users))
	for i := range f.users {
		byID[f.users[i].id] = &f.users[i]
	}

	for range 2000 {
		sess := f.session(start, end)
		u, ok := byID[sess[0].distinctID]
		if !ok {
			continue // bot session
		}
		first := sess[0].occurTime
		last := sess[len(sess)-1].occurTime
		if first.Before(u.join) {
			t.Fatalf("user %s session at %v before join %v", u.id, first, u.join)
		}
		if last.After(u.churn.Add(time.Hour)) {
			t.Fatalf("user %s session ends %v, churned %v", u.id, last, u.churn)
		}
	}
}

// Live sessions must start at the anchor time and proceed strictly into the
// future in order, so the demo worker can play them back in real time.
func TestLiveSessionPlayableInRealTime(t *testing.T) {
	gen := NewLiveGenerator()
	now := time.Now()

	for range 500 {
		sess := gen.LiveSession(now)
		if len(sess) == 0 {
			t.Fatal("empty live session")
		}
		if !sess[0].OccurTime.Equal(now) {
			t.Fatalf("first event at %v, want anchor %v", sess[0].OccurTime, now)
		}
		for i := 1; i < len(sess); i++ {
			if sess[i].OccurTime.Before(sess[i-1].OccurTime) {
				t.Fatalf("event %d (%s) at %v before event %d at %v",
					i, sess[i].Kind, sess[i].OccurTime, i-1, sess[i-1].OccurTime)
			}
		}
		last := sess[len(sess)-1].OccurTime
		if last.After(now.Add(time.Hour)) {
			t.Fatalf("session extends too far into the future: %v", last.Sub(now))
		}
	}
}

func TestTrafficFactorRange(t *testing.T) {
	// 1.0 is the normal busiest hour; episodes (promo weeks, push blasts)
	// push the factor above 1, capped by the promo multiplier.
	for h := range 24 * 90 { // sweep ~2 episode cycles
		ts := time.Date(2026, 4, 1, h, 0, 0, 0, time.UTC)
		f := TrafficFactor(ts)
		if f <= 0 || f > promoTrafficMult {
			t.Fatalf("TrafficFactor(%v) = %v, want in (0, %v]", ts, f, promoTrafficMult)
		}
	}
}

// Promo episodes must be deterministic and actually move traffic.
func TestEpisodesDeterministicAndActive(t *testing.T) {
	insidePromo := userJoinEpoch.Add(3*episodeCycle + promoStart + promoLen/2)
	if name, ok := activePromo(insidePromo); !ok || name == "" {
		t.Fatalf("expected an active promo at %v", insidePromo)
	}
	if m := episodeTrafficMult(insidePromo); m != promoTrafficMult {
		t.Fatalf("promo traffic mult = %v, want %v", m, promoTrafficMult)
	}
	outside := userJoinEpoch.Add(3 * episodeCycle) // day 0 of a cycle
	if _, ok := activePromo(outside); ok {
		t.Fatalf("unexpected promo at cycle start %v", outside)
	}
	if m := episodeTrafficMult(outside); m != 1 {
		t.Fatalf("baseline traffic mult = %v, want 1", m)
	}
}

// App version adoption: picks must be released versions, and the latest
// version must dominate a few weeks after its release.
func TestAppVersionAdoption(t *testing.T) {
	at := userJoinEpoch.Add(10*appReleaseInterval + 21*24*time.Hour)
	released := appReleases[:releasedCount(at)]
	latest := latestAppVersionAt(at)

	counts := map[string]int{}
	for range 5000 {
		v := appVersionAt(at)
		counts[v]++
		found := false
		for _, rv := range released {
			if v == rv {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("appVersionAt returned unreleased version %s", v)
		}
	}
	if counts[latest] < 1500 { // newest should clearly dominate 3 weeks in
		t.Fatalf("latest version %s picked only %d/5000 times", latest, counts[latest])
	}
}

// Cross-session memory: a refund after a purchase must cite the real order,
// a trial conversion must reuse the started trial, and push recovery must
// resume the abandoned cart.
func TestCrossSessionContinuity(t *testing.T) {
	f := newSessionFactory()
	u := &f.users[2] // member (2%10 < 3)
	if !u.member {
		t.Fatal("expected user 2 to be a member")
	}
	prof := deviceProfiles[0] // desktop web
	mem := f.memory(u)
	t0 := u.join.Add(30 * 24 * time.Hour)

	// Purchase, then refund a week later: order ids and amounts must match.
	purchase := buildSession(u, prof, journeyByName(webJourneys, "purchase"), t0, t0.Add(time.Hour), mem)
	var orderID string
	var amount float64
	for _, e := range purchase {
		if e.kind == "purchase" {
			orderID = e.customProperties["order_id"].(string)
			amount = e.customProperties["amount"].(float64)
		}
	}
	if orderID == "" {
		t.Fatal("purchase journey produced no purchase event")
	}
	refund := buildSession(u, prof, journeyByName(webJourneys, "refund"), t0.Add(7*24*time.Hour), t0.Add(8*24*time.Hour), mem)
	for _, e := range refund {
		if e.kind == "order_refunded" {
			if got := e.customProperties["order_id"].(string); got != orderID {
				t.Fatalf("refund cites order %s, want %s", got, orderID)
			}
			if got := e.customProperties["amount"].(float64); got != amount {
				t.Fatalf("refund amount %v, want %v", got, amount)
			}
		}
	}

	// Abandon a cart, then recover it via push: same cart total.
	abandon := buildSession(u, prof, journeyByName(webJourneys, "abandoned-cart"), t0.Add(14*24*time.Hour), t0.Add(15*24*time.Hour), mem)
	var abandonedAmount float64
	for _, e := range abandon {
		if e.kind == "cart_viewed" {
			abandonedAmount = e.customProperties["amount"].(float64)
		}
	}
	appProf := deviceProfiles[len(deviceProfiles)-1] // android app
	recover := buildSession(u, appProf, journeyByName(appJourneys, "push-recovery"), t0.Add(15*24*time.Hour), t0.Add(16*24*time.Hour), mem)
	for _, e := range recover {
		if e.kind == "cart_viewed" {
			if got := e.customProperties["amount"].(float64); got != abandonedAmount {
				t.Fatalf("recovered cart amount %v, want abandoned %v", got, abandonedAmount)
			}
		}
	}

	// Trial started, then converted: trial id carries over.
	trial := buildSession(u, prof, journeyByName(webJourneys, "club-trial"), t0.Add(20*24*time.Hour), t0.Add(21*24*time.Hour), mem)
	var trialID string
	for _, e := range trial {
		if e.kind == "trial_started" {
			trialID = e.customProperties["trial_id"].(string)
		}
	}
	convert := buildSession(u, prof, journeyByName(webJourneys, "club-convert"), t0.Add(25*24*time.Hour), t0.Add(26*24*time.Hour), mem)
	for _, e := range convert {
		if e.kind == "trial_converted" {
			if got := e.customProperties["trial_id"].(string); got != trialID {
				t.Fatalf("conversion cites trial %s, want %s", got, trialID)
			}
		}
	}
}

// Purchase funnels must be internally coherent: the purchase amount equals
// the cart total minus any coupon discount, and ids stay consistent.
func TestSessionFunnelCoherence(t *testing.T) {
	end := time.Now()
	start := end.AddDate(0, -1, 0)
	factory := newSessionFactory()

	checked := 0
	for range 20_000 {
		sess := factory.session(start, end)

		var cartTotal, discount float64
		var purchase map[string]any
		for _, e := range sess {
			switch e.kind {
			case "add_to_cart":
				cartTotal += e.customProperties["price"].(float64) * float64(e.customProperties["quantity"].(int))
			case "coupon_applied":
				discount = e.customProperties["discount_amount"].(float64)
			case "purchase":
				purchase = e.customProperties
			}
		}
		// Only check funnels that passed through an explicit add_to_cart
		// (recovery journeys build their cart implicitly).
		if purchase == nil || cartTotal == 0 {
			continue
		}
		checked++

		want := cartTotal - discount
		if want < 0.99 {
			want = 0.99
		}
		got := purchase["amount"].(float64)
		if diff := got - want; diff > 0.01 || diff < -0.01 {
			t.Fatalf("purchase amount %v, want %v (cart %v - discount %v)", got, want, cartTotal, discount)
		}
		if purchase["currency"] != "USD" {
			t.Fatalf("purchase currency %v, want USD", purchase["currency"])
		}
	}
	if checked == 0 {
		t.Fatal("no purchase funnels generated in 20k sessions")
	}
}

// TestUserMemoryCausality pins the load-bearing invariant of the cross-session
// memory helpers: a remembered entry is released only to a session that
// postdates it. This is what stops out-of-order batch generation from producing
// a refund that predates its order or a conversion before its trial. The
// existing continuity test only consumes in-order, so it never exercises the
// guard these helpers exist for.
func TestUserMemoryCausality(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	m := &userMemory{}
	m.rememberOrder(pastOrder{id: "ord-1", amount: 10, t: t0})
	if _, ok := m.takeOrderBefore(t0.Add(-time.Hour)); ok {
		t.Error("takeOrderBefore released an order that postdates the cutoff")
	}
	if got, ok := m.takeOrderBefore(t0.Add(time.Hour)); !ok || got.id != "ord-1" {
		t.Errorf("takeOrderBefore = %+v, %v; want ord-1, true", got, ok)
	}

	m = &userMemory{}
	m.rememberAbandonedCart([]cartLine{{p: catalog[0], qty: 1}}, t0)
	if _, ok := m.takeAbandonedCartBefore(t0.Add(-time.Hour)); ok {
		t.Error("takeAbandonedCartBefore released a cart that postdates the cutoff")
	}
	if _, ok := m.takeAbandonedCartBefore(t0.Add(time.Hour)); !ok {
		t.Error("takeAbandonedCartBefore withheld a cart that predates the cutoff")
	}

	m = &userMemory{}
	m.rememberTrial("trial-1", t0)
	if _, ok := m.takeTrialBefore(t0.Add(-time.Hour)); ok {
		t.Error("takeTrialBefore released a trial that postdates the cutoff")
	}
	if id, ok := m.takeTrialBefore(t0.Add(time.Hour)); !ok || id != "trial-1" {
		t.Errorf("takeTrialBefore = %q, %v; want trial-1, true", id, ok)
	}
}

// TestUserMemoryOrderCap pins the bounded-memory invariant that keeps the live
// worker's per-user state flat: only the most recent maxRememberedOrders are
// retained, oldest-first.
func TestUserMemoryOrderCap(t *testing.T) {
	m := &userMemory{}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range maxRememberedOrders + 2 {
		m.rememberOrder(pastOrder{id: fmt.Sprintf("ord-%d", i), amount: float64(i), t: base.Add(time.Duration(i) * time.Hour)})
	}
	if len(m.orders) != maxRememberedOrders {
		t.Fatalf("orders len = %d, want cap %d", len(m.orders), maxRememberedOrders)
	}
	if m.orders[0].id != "ord-2" { // the two oldest were dropped
		t.Errorf("oldest surviving order = %s, want ord-2", m.orders[0].id)
	}
}

// TestWeightedIndexDegenerate pins the degenerate-weight contract: an all-zero
// (or empty) weight table returns index 0 rather than panicking in rand.IntN(0),
// and the normal selection arithmetic lands in the expected bucket.
func TestWeightedIndexDegenerate(t *testing.T) {
	if got := weightedIndex([]int{0, 0, 0}); got != 0 {
		t.Errorf("weightedIndex(all-zero) = %d, want 0", got)
	}
	r := rand.New(rand.NewPCG(1, 2))
	if got := weightedIndexR(r, []int{0, 0}); got != 0 {
		t.Errorf("weightedIndexR(all-zero) = %d, want 0", got)
	}
	if got := weightedIndexN(func(int) int { return 2 }, []int{1, 1, 1}); got != 2 {
		t.Errorf("weightedIndexN({1,1,1}, pick=2) = %d, want 2", got)
	}
}

// TestPickActiveUserDegenerateWindow pins that the lifecycle-rejection loop
// always makes progress: a window with no overlapping user still returns a
// usable user and a non-empty window (the documented fallback), and a normal
// window returns an overlap contained in the user's lifecycle.
func TestPickActiveUserDegenerateWindow(t *testing.T) {
	f := newSessionFactory()

	before := userJoinEpoch.Add(-365 * 24 * time.Hour)
	u, s, e := f.pickActiveUser(before, before.Add(time.Minute))
	if u == nil {
		t.Fatal("pickActiveUser returned nil for a pre-epoch window")
	}
	if !e.After(s) {
		t.Fatalf("pickActiveUser returned empty window [%v, %v]", s, e)
	}

	mid := userJoinEpoch.Add(400 * 24 * time.Hour)
	u, s, e = f.pickActiveUser(mid, mid.Add(time.Hour))
	if u == nil || !e.After(s) {
		t.Fatalf("pickActiveUser(normal) = %v, [%v, %v]", u, s, e)
	}
	if s.Before(u.join) || e.After(u.churn) {
		t.Fatalf("overlap [%v, %v] escapes user lifecycle [%v, %v]", s, e, u.join, u.churn)
	}
}

// TestPickJourneyMemberOnly pins the membership gate: a non-member can never be
// assigned a member-only journey (push, billing, NPS), while a member can.
func TestPickJourneyMemberOnly(t *testing.T) {
	for _, platform := range []string{"web", "ios", "android"} {
		for range 5000 {
			if jd := pickJourney(platform, false); jd.memberOnly {
				t.Fatalf("pickJourney(%s, member=false) returned member-only journey %q", platform, jd.name)
			}
		}
	}
	sawMemberOnly := false
	for range 5000 {
		if pickJourney("web", true).memberOnly {
			sawMemberOnly = true
			break
		}
	}
	if !sawMemberOnly {
		t.Error("pickJourney(web, member=true) never returned a member-only journey in 5000 draws")
	}
}

// TestBotSessionShape pins that crawler sessions stay web-only, carry a low bot
// score, use bot- distinct ids, and never emit commerce events — so bot traffic
// can't pollute revenue/funnel charts.
func TestBotSessionShape(t *testing.T) {
	f := newSessionFactory()
	now := time.Now()
	for range 500 {
		sess := f.botSession(now, now.Add(time.Hour))
		if len(sess) == 0 {
			t.Fatal("empty bot session")
		}
		for _, e := range sess {
			a := e.autoProperties
			if a["$platform"] != "web" {
				t.Fatalf("bot event on non-web platform %v", a["$platform"])
			}
			if score, ok := a["$bot_score"].(int); !ok || score < 1 || score > 28 {
				t.Fatalf("bot $bot_score = %v, want 1-28", a["$bot_score"])
			}
			if !strings.HasPrefix(e.distinctID, "bot-") {
				t.Fatalf("bot distinct id = %q, want bot- prefix", e.distinctID)
			}
			switch e.kind {
			case "purchase", "add_to_cart", "checkout_started", "trial_started":
				t.Fatalf("bot emitted commerce event %q", e.kind)
			}
		}
	}
}

// TestToLiveEventsCopiesAllFields pins the event → LiveEvent boundary: every
// field round-trips, and a field-count check guards against a new event field
// silently going unmapped (which would diverge backfilled events from live ones).
func TestToLiveEventsCopiesAllFields(t *testing.T) {
	e := event{
		eventID:          "evt",
		distinctID:       "did",
		sessionID:        "sid",
		kind:             "purchase",
		occurTime:        time.Unix(1_700_000_000, 0).UTC(),
		autoProperties:   map[string]any{"$k": "v"},
		customProperties: map[string]any{"amount": 1.0},
	}
	got := toLiveEvents([]event{e})
	if len(got) != 1 {
		t.Fatalf("toLiveEvents len = %d, want 1", len(got))
	}
	le := got[0]
	if le.EventID != e.eventID || le.DistinctID != e.distinctID || le.SessionID != e.sessionID ||
		le.Kind != e.kind || !le.OccurTime.Equal(e.occurTime) {
		t.Fatalf("scalar field mismatch: %+v vs %+v", le, e)
	}
	if le.AutoProperties["$k"] != "v" || le.CustomProperties["amount"] != 1.0 {
		t.Fatalf("property maps not copied: %+v", le)
	}
	if ne, nl := reflect.TypeOf(event{}).NumField(), reflect.TypeOf(LiveEvent{}).NumField(); ne != nl {
		t.Errorf("event has %d fields, LiveEvent has %d — toLiveEvents must map all of them", ne, nl)
	}
}

// TestHumanUserIndex pins the distinct-id → user-index parse the backfill uses
// to record which users produced events: human ids in range resolve; bot ids,
// junk, trailing garbage and out-of-range indices do not (so a bot never gets a
// profile and ok=true is safe to feed straight into DemoUserAt).
func TestHumanUserIndex(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"user-00000", 0, true},
		{"user-00042", 42, true},
		{"user-09999", 9999, true},
		{"bot-0001", 0, false},
		{"", 0, false},
		{"user-", 0, false},
		{"user-10000", 0, false},    // == pool size: out of range
		{"user-99999999", 0, false}, // far out of range
		{"user-12x", 0, false},      // trailing garbage
		{"user--1", 0, false},       // negative
	}
	for _, c := range cases {
		got, ok := HumanUserIndex(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("HumanUserIndex(%q) = %d, %v; want %d, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	// Every in-range index that ok=true reports must be safe for DemoUserAt
	// (no out-of-range panic), since the live worker chains the two.
	for _, id := range []string{"user-00000", "user-09999"} {
		idx, ok := HumanUserIndex(id)
		if !ok {
			t.Fatalf("HumanUserIndex(%q) unexpectedly not ok", id)
		}
		_ = DemoUserAt(idx) // must not panic
	}
}

// TestRecordActiveUser pins the recording decision the no-profile-without-events
// guarantee rests on: a human id is recorded, a bot or out-of-range id is not,
// and a nil set is a no-op (the CLI path passes nil).
func TestRecordActiveUser(t *testing.T) {
	active := map[int]struct{}{}
	recordActiveUser(active, "user-00042")
	recordActiveUser(active, "bot-0001")   // bots never get a profile
	recordActiveUser(active, "user-99999") // out of range: not a valid human id
	recordActiveUser(active, "garbage")    // junk
	if _, ok := active[42]; !ok || len(active) != 1 {
		t.Fatalf("active = %v, want exactly {42}", active)
	}
	recordActiveUser(nil, "user-00001") // must not panic
}

// TestAutoAnyMapToVariantMap pins the Go-type → ClickHouse-Variant routing the
// backfill and live insert both share (the contract the deleted proto-mapping
// tests used to guard, relocated here). A mis-slotted type would silently
// mistype a ClickHouse column on every demo event.
func TestAutoAnyMapToVariantMap(t *testing.T) {
	ctx := context.Background()
	out := autoAnyMapToVariantMap(ctx, "proj", map[string]any{
		"i":    7,           // int   → Int64
		"i64":  int64(9),    // int64 → Int64
		"b":    true,        // bool  → Bool
		"f":    3.14,        // float → Float64
		"plan": "pro",       // custom string → String
		"junk": []int{1, 2}, // unhandled → String
	})
	want := map[string]struct {
		chType string
		value  any
	}{
		"i":    {"Int64", int64(7)},
		"i64":  {"Int64", int64(9)},
		"b":    {"Bool", true},
		"f":    {"Float64", 3.14},
		"plan": {"String", "pro"},
		"junk": {"String", "[1 2]"},
	}
	for k, w := range want {
		v, ok := out[k]
		if !ok {
			t.Errorf("key %q missing from variant map", k)
			continue
		}
		if v.Type() != w.chType || v.Any() != w.value {
			t.Errorf("key %q = %s(%v), want %s(%v)", k, v.Type(), v.Any(), w.chType, w.value)
		}
	}
	// Empty/nil input yields a nil map so the column is omitted on the wire.
	if got := autoAnyMapToVariantMap(ctx, "proj", nil); got != nil {
		t.Errorf("autoAnyMapToVariantMap(nil) = %v, want nil", got)
	}
	if got := autoAnyMapToVariantMap(ctx, "proj", map[string]any{}); got != nil {
		t.Errorf("autoAnyMapToVariantMap(empty) = %v, want nil", got)
	}

	// Non-finite floats stay in the Float64 slot (ClickHouse Float64 carries
	// nan/inf natively). This is a deliberate behavior change from the old proto
	// path, which coerced them to String — a silent column-type flip here would
	// mistype the property on every affected event.
	for _, nf := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		got := autoAnyMapToVariantMap(ctx, "proj", map[string]any{"nf": nf})
		if v, ok := got["nf"]; !ok || v.Type() != "Float64" {
			t.Errorf("non-finite %v slotted as %q, want Float64", nf, got["nf"].Type())
		}
	}
}

// TestPageViewInjectedOnWebNavigation pins the storefront page_view emission: a
// web step that lands on a new page gets a page_view injected immediately before
// it carrying the same url (so page_view stays the dominant kind), while a step
// that doesn't move the page (scroll) gets none. In browse, product_list_viewed
// always navigates to a collection page and scroll always follows it without
// moving, so both are deterministic regardless of which products are sampled.
func TestPageViewInjectedOnWebNavigation(t *testing.T) {
	f := newSessionFactory()
	u := &f.users[2]
	web := deviceProfiles[0] // desktop web
	t0 := u.join.Add(30 * 24 * time.Hour)

	sess := buildSession(u, web, journeyByName(webJourneys, "browse"), t0, t0.Add(time.Hour), f.memory(u))

	// product_list_viewed always changes the page, so it is preceded by an
	// injected page_view sharing its url (the explicit home page_view sits on
	// "/", so a matching-url predecessor is necessarily the injected one).
	j := indexOfKind(sess, "product_list_viewed")
	if j < 1 || sess[j-1].kind != "page_view" {
		t.Fatalf("product_list_viewed (at %d) not preceded by a page_view: %v", j, kindsOf(sess))
	}
	if sess[j-1].autoProperties["$url"] != sess[j].autoProperties["$url"] {
		t.Errorf("injected page_view url %v != following event url %v",
			sess[j-1].autoProperties["$url"], sess[j].autoProperties["$url"])
	}
	// scroll doesn't move the page (it follows product_list_viewed directly), so
	// no page_view is injected for it.
	k := indexOfKind(sess, "scroll")
	if k < 1 || sess[k-1].kind == "page_view" {
		t.Errorf("scroll (at %d) was preceded by a page_view; scroll must not inject one: %v", k, kindsOf(sess))
	}
}

func indexOfKind(sess []event, kind string) int {
	for i, e := range sess {
		if e.kind == kind {
			return i
		}
	}
	return -1
}

func kindsOf(sess []event) []string {
	out := make([]string, len(sess))
	for i, e := range sess {
		out[i] = e.kind
	}
	return out
}

// TestPageViewNotInjectedOnApp pins that the page_view injection is web-only:
// an app session never gets synthetic page_view events.
func TestPageViewNotInjectedOnApp(t *testing.T) {
	f := newSessionFactory()
	u := &f.users[2]
	app := deviceProfiles[len(deviceProfiles)-1] // android app
	t0 := u.join.Add(30 * 24 * time.Hour)

	sess := buildSession(u, app, journeyByName(appJourneys, "app-browse"), t0, t0.Add(time.Hour), f.memory(u))
	for _, e := range sess {
		if e.kind == "page_view" {
			t.Fatalf("app session emitted a page_view (web-only injection leaked)")
		}
	}
}

// TestDemoUserAtPanics pins the documented out-of-range contract DemoUserAt
// relies on as a fail-fast (the live worker treats a bad index as a bug).
func TestDemoUserAtPanics(t *testing.T) {
	for _, i := range []int{-1, DistinctIDPool} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("DemoUserAt(%d) did not panic", i)
				}
			}()
			_ = DemoUserAt(i)
		}()
	}
}

// TestSignupForcedOnce pins the signup cap: a user's first in-window session is
// forced onto the signup/install journey, but a second in-window session is not
// — so a user emits at most one signup and acquisition counts aren't inflated.
func TestSignupForcedOnce(t *testing.T) {
	f := newSessionFactory()
	u := &f.users[0]
	prof := u.devices[0]
	inWindow := u.join.Add(time.Hour) // within firstSessionWindow of join

	jd1 := f.journeyFor(u, prof, inWindow)
	if jd1.name != webSignupJourney.name && jd1.name != appInstallJourney.name {
		t.Fatalf("first in-window journey = %q, want a signup/install journey", jd1.name)
	}
	// Building the session records the signup.
	buildSession(u, prof, jd1, inWindow, inWindow.Add(time.Hour), f.memory(u))

	jd2 := f.journeyFor(u, prof, inWindow.Add(2*time.Hour)) // still in window
	if jd2.name == webSignupJourney.name || jd2.name == appInstallJourney.name {
		t.Fatalf("second in-window session re-forced signup journey %q", jd2.name)
	}
}

// TestActiveSetCollection pins the invariant the no-profile-without-events
// guarantee rests on: the users a backfill emits events for are all real,
// in-range pool members that joined before the window end — never a future-join
// user and never a bot.
func TestActiveSetCollection(t *testing.T) {
	f := newSessionFactory()
	end := time.Now()
	start := end.AddDate(0, -4, 0)

	active := map[int]struct{}{}
	for range 5000 {
		sess := f.session(start, end)
		if len(sess) == 0 {
			continue
		}
		if idx, ok := HumanUserIndex(sess[0].distinctID); ok {
			active[idx] = struct{}{}
		}
	}
	if len(active) == 0 {
		t.Fatal("no active users collected in 5000 sessions")
	}
	for idx := range active {
		if idx < 0 || idx >= DistinctIDPool {
			t.Fatalf("active index %d out of range [0,%d)", idx, DistinctIDPool)
		}
		if f.users[idx].join.After(end) {
			t.Fatalf("active user %d joined %v, after window end %v (future-join users must stay event-less)",
				idx, f.users[idx].join, end)
		}
	}
}

// TestTrafficFactorShape pins the direction of the diurnal curve (not just its
// range): the small hours are quieter than the evening peak on a given day. A
// transposed hour table (peak at 3am) passes the range test but fails this.
func TestTrafficFactorShape(t *testing.T) {
	day := time.Date(2026, 4, 7, 0, 0, 0, 0, time.UTC) // Tuesday
	night := TrafficFactor(day.Add(3 * time.Hour))
	evening := TrafficFactor(day.Add(20 * time.Hour))
	if night >= evening {
		t.Errorf("TrafficFactor 03:00 (%v) >= 20:00 (%v); diurnal curve looks inverted", night, evening)
	}
}
