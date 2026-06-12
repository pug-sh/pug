package seed

import (
	"testing"
	"time"
)

func TestInferKind(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "view", want: "page_view"},
		{input: "cart", want: "add_to_cart"},
		{input: "purchase", want: "purchase"},
		{input: "order-123", want: "purchase"},
		{input: "", want: "page_view"},
	}

	for _, tt := range tests {
		if got := inferKind(tt.input); got != tt.want {
			t.Errorf("inferKind(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsEventType(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{input: "view", want: true},
		{input: "cart", want: true},
		{input: "purchase", want: true},
		{input: "signup", want: false},
		{input: "order-123", want: false},
	}

	for _, tt := range tests {
		if got := isEventType(tt.input); got != tt.want {
			t.Errorf("isEventType(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

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
