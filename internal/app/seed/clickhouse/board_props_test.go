package seed

import (
	"fmt"
	"testing"
	"time"
)

var pageTypes = map[string]bool{
	"Home": true, "Collections": true, "Product": true, "Search": true, "Cart": true,
	"Checkout": true, "Confirmation": true, "Guide": true, "Club": true, "Account": true,
}

func TestWebEventsCarryPageType(t *testing.T) {
	f := newSessionFactory()
	end := time.Now()
	start := end.AddDate(0, -1, 0)

	landings := map[string]int{}
	viewed := map[string]bool{}
	webSessions := 0
	for range 4000 {
		sess := f.session(t.Context(), start, end)
		if len(sess) == 0 || sess[0].autoProperties["$platform"] != "web" {
			continue
		}
		for _, e := range sess {
			pt, _ := e.customProperties["page_type"].(string)
			if !pageTypes[pt] {
				t.Fatalf("web %s event has page_type %q", e.kind, pt)
			}
			if e.kind == "page_view" {
				viewed[pt] = true
			}
		}
		if _, human := HumanUserIndex(sess[0].distinctID); human {
			webSessions++
			landings[sess[0].customProperties["page_type"].(string)]++
		}
	}
	if webSessions == 0 {
		t.Fatal("no human web sessions generated")
	}
	for pt := range pageTypes {
		if !viewed[pt] {
			t.Errorf("no page_view has page_type %s", pt)
		}
	}
	for _, pt := range []string{"Product", "Collections", "Guide", "Search"} {
		if landings[pt]*100 < webSessions*3 {
			t.Errorf("%d of %d web sessions land on %s, want at least 3%%: %v", landings[pt], webSessions, pt, landings)
		}
	}
}

func TestPurchaseCarriesBoardProperties(t *testing.T) {
	methods := map[string]map[string]bool{
		"web":     {"Card": true, "PayPal": true, "Apple Pay": true, "Google Pay": true},
		"ios":     {"Apple Pay": true, "Card": true, "PayPal": true},
		"android": {"Google Pay": true, "Card": true, "PayPal": true},
	}
	f := newSessionFactory()
	end := time.Now()
	start := end.AddDate(0, -1, 0)

	checked := 0
	for range 20_000 {
		for _, e := range f.session(t.Context(), start, end) {
			if e.kind != "purchase" {
				continue
			}
			checked++
			c := e.customProperties
			platform := e.autoProperties["$platform"].(string)
			if name, _ := c["product_name"].(string); name == "" {
				t.Fatalf("purchase without product_name: %v", c)
			}
			if m, _ := c["payment_method"].(string); !methods[platform][m] {
				t.Fatalf("%s purchase has payment_method %q", platform, m)
			}
			idx, ok := HumanUserIndex(e.distinctID)
			if !ok {
				t.Fatalf("purchase from non-human %q", e.distinctID)
			}
			want := "Non-member"
			if f.users[idx].member {
				want = "Member"
			}
			if c["membership"] != want {
				t.Fatalf("membership %v, want %q to match the user", c["membership"], want)
			}
			if platform == "web" && c["page_type"] != "Confirmation" {
				t.Fatalf("web purchase on page_type %v, want Confirmation", c["page_type"])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no purchases generated in 20k sessions")
	}
}

func TestBotProfilesTagLikeIngest(t *testing.T) {
	want := []string{"Googlebot", "bingbot", "AhrefsBot", "SemrushBot", "GPTBot", "claudebot", "PerplexityBot", "asn:16509", "screen:800x600"}
	if len(botProfiles) != len(want) {
		t.Fatalf("%d bot profiles, want %d", len(botProfiles), len(want))
	}
	for i, bp := range botProfiles {
		if reason, _ := botTag(bp); reason != want[i] {
			t.Errorf("bot profile %d (%q) tags as %q, want %q", i, bp.browser, reason, want[i])
		}
	}
	f := newSessionFactory()
	now := time.Now()
	for range 300 {
		for _, e := range f.botSession(now, now.Add(time.Hour)) {
			if e.autoProperties["$bot"] != true {
				t.Fatalf("bot event without $bot: %v", e.autoProperties)
			}
			if r, _ := e.autoProperties["$bot_reason"].(string); r == "" {
				t.Fatalf("bot event without $bot_reason: %v", e.autoProperties)
			}
		}
	}
}

func TestAppAndClubEventProperties(t *testing.T) {
	f := newSessionFactory()
	end := time.Now()
	start := end.AddDate(0, -1, 0)

	opens, invoices := 0, 0
	for range 20_000 {
		for _, e := range f.session(t.Context(), start, end) {
			switch e.kind {
			case "app_open":
				opens++
				if ms, _ := e.customProperties["cold_start_ms"].(int); ms <= 0 {
					t.Fatalf("app_open cold_start_ms = %v", e.customProperties["cold_start_ms"])
				}
			case "invoice_paid":
				invoices++
				if name, _ := e.customProperties["plan_name"].(string); name == "" {
					t.Fatalf("invoice_paid without plan_name: %v", e.customProperties)
				}
			}
		}
	}
	if opens == 0 || invoices == 0 {
		t.Fatalf("generated %d app opens and %d invoices; want both", opens, invoices)
	}
}

func TestTrialsConvertOnce(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	m := &userMemory{}
	m.rememberTrial("trial-1", t0, 1)
	if m.decideTrial(t0.Add(24*time.Hour)) || m.decideTrial(t0.Add(20*24*time.Hour)) || m.trialID == "" {
		t.Fatal("trial decided outside 3 to 14 days")
	}

	converted := 0
	const trials = 4000
	for i := range trials {
		m := &userMemory{}
		m.rememberTrial(fmt.Sprintf("trial-%d", i), t0, 1)
		if m.decideTrial(t0.Add(5 * 24 * time.Hour)) {
			converted++
			if !m.subscribed {
				t.Fatal("a converted trial left the member unsubscribed")
			}
			continue
		}
		if m.trialID != "" {
			t.Fatal("open trial neither converted nor lapsed at day 5")
		}
		if m.decideTrial(t0.Add(6 * 24 * time.Hour)) {
			t.Fatal("a lapsed trial converted later")
		}
	}
	if rate := float64(converted) / trials; rate < 0.40 || rate > 0.50 {
		t.Errorf("trial conversion %.2f, want about 0.45", rate)
	}
}

func TestScrollDepthFollowsPageType(t *testing.T) {
	avg := func(path string) float64 {
		var sum int
		for range 4000 {
			sum += scrollDepth("web", path)
		}
		return float64(sum) / 4000
	}
	if guide, cart := avg("/help/harness-sizing"), avg("/cart"); guide < cart+30 {
		t.Errorf("guides average %.0f%% scrolled, carts %.0f%%; want guides well ahead", guide, cart)
	}
}
