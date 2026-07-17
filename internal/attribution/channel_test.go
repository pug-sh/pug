package attribution

import "testing"

// TestClassifyChannelRules pins the normative rule table row by row, plus the
// precedence between rows. The full Input → Derive path is exercised so the
// tests also cover UTM completion, self-blanking, and lowercasing feeding the
// classifier. Editing an expectation here is a taxonomy change — update
// docs/architecture/web-analytics.md alongside.
func TestClassifyChannelRules(t *testing.T) {
	page := "https://pugandpals.example.com/products/ball"
	cases := []struct {
		name string
		in   Input
		want string
	}{
		// 1: Paid Search
		{"paid search via cpc+google source", Input{URL: page, UTMSource: "google", UTMMedium: "cpc"}, ChannelPaidSearch},
		{"paid search via ref domain", Input{URL: page, Referrer: "https://www.google.co.uk/search", UTMMedium: "ppc"}, ChannelPaidSearch},
		{"paid search via bing subdomain ref", Input{URL: page, Referrer: "https://cn.bing.com", UTMMedium: "cpc"}, ChannelPaidSearch},
		// 2: Paid Social
		{"paid social via paid_social medium", Input{URL: page, UTMSource: "facebook", UTMMedium: "paid_social"}, ChannelPaidSocial},
		{"paid social via retargeting + ref", Input{URL: page, Referrer: "https://l.facebook.com/l.php", UTMMedium: "retargeting"}, ChannelPaidSocial},
		// 3: Paid Video
		{"paid video", Input{URL: page, UTMSource: "youtube", UTMMedium: "cpv"}, ChannelPaidVideo},
		{"paid video via youtu.be ref", Input{URL: page, Referrer: "https://youtu.be/abc", UTMMedium: "cpc"}, ChannelPaidVideo},
		// 4: Display
		{"display medium", Input{URL: page, UTMSource: "gdn", UTMMedium: "display"}, ChannelDisplay},
		{"banner medium", Input{URL: page, UTMMedium: "banner"}, ChannelDisplay},
		{"cpm with unmatched source is display", Input{URL: page, UTMSource: "adnetwork", UTMMedium: "cpm"}, ChannelDisplay},
		// 1 beats 4: cpm with a search source is Paid Search (rule order).
		{"cpm with search source is paid search", Input{URL: page, UTMSource: "google", UTMMedium: "cpm"}, ChannelPaidSearch},
		// 5: Paid Other
		{"paid other", Input{URL: page, UTMSource: "partnerx", UTMMedium: "cpc"}, ChannelPaidOther},
		{"paid prefix medium", Input{URL: page, UTMSource: "partnerx", UTMMedium: "paid-placement"}, ChannelPaidOther},
		// 6: Organic Search
		{"organic search via ref", Input{URL: page, Referrer: "https://www.google.com/"}, ChannelOrganicSearch},
		{"organic search via duckduckgo ref", Input{URL: page, Referrer: "https://duckduckgo.com/"}, ChannelOrganicSearch},
		{"organic search via perplexity ref", Input{URL: page, Referrer: "https://www.perplexity.ai/"}, ChannelOrganicSearch},
		{"organic medium", Input{URL: page, UTMSource: "seo-tool", UTMMedium: "organic"}, ChannelOrganicSearch},
		{"organic search via google ccTLD subdomain", Input{URL: page, Referrer: "https://images.google.co.in/x"}, ChannelOrganicSearch},
		// 7: Organic Social
		{"organic social via ref", Input{URL: page, Referrer: "https://reddit.com/r/pugs"}, ChannelOrganicSocial},
		{"organic social via t.co", Input{URL: page, Referrer: "https://t.co/abc"}, ChannelOrganicSocial},
		{"organic social via hn", Input{URL: page, Referrer: "https://news.ycombinator.com/item?id=1"}, ChannelOrganicSocial},
		{"organic social via medium sm", Input{URL: page, UTMSource: "buffer", UTMMedium: "sm"}, ChannelOrganicSocial},
		{"organic social via source", Input{URL: page, UTMSource: "instagram"}, ChannelOrganicSocial},
		// 8: Organic Video
		{"organic video via ref", Input{URL: page, Referrer: "https://www.youtube.com/watch?v=1"}, ChannelOrganicVideo},
		{"organic video via medium", Input{URL: page, UTMSource: "sponsor", UTMMedium: "video"}, ChannelOrganicVideo},
		// 2 and 3 beat 4: cpm is a paid medium, so a social/video source takes
		// its own paid bucket rather than falling into Display.
		{"cpm with social source is paid social", Input{URL: page, UTMSource: "facebook", UTMMedium: "cpm"}, ChannelPaidSocial},
		{"cpm with video source is paid video", Input{URL: page, UTMSource: "youtube", UTMMedium: "cpm"}, ChannelPaidVideo},
		// 9: Email
		{"email via medium", Input{URL: page, UTMSource: "lifecycle", UTMMedium: "email"}, ChannelEmail},
		{"email via newsletter source", Input{URL: page, UTMSource: "newsletter"}, ChannelEmail},
		{"email via e-mail medium variant", Input{URL: page, UTMMedium: "E-Mail"}, ChannelEmail},
		// 5 beats 9: a paid medium wins even when the source reads as email.
		{"paid medium with newsletter source is paid other", Input{URL: page, UTMSource: "newsletter", UTMMedium: "cpc"}, ChannelPaidOther},
		// 8 beats 9: a video source wins over an email-shaped medium.
		{"video source with newsletter medium is organic video", Input{URL: page, UTMSource: "youtube", UTMMedium: "newsletter"}, ChannelOrganicVideo},
		// 10: Affiliate
		{"affiliate", Input{URL: page, UTMSource: "partner", UTMMedium: "affiliate"}, ChannelAffiliate},
		// 9 and 10 beat 11. Every other email/affiliate case above omits a
		// referrer, which is the shape that cannot happen in production: a real
		// affiliate click carries the affiliate site as document.referrer and a
		// newsletter click often carries a webmail one. Without these, moving
		// rule 11 above 9/10 would silently reclassify both as Referral into a
		// permanent rollup dimension with the whole suite still green.
		{"affiliate with referrer beats referral", Input{URL: page, Referrer: "https://partner.example.org/review", UTMMedium: "affiliate"}, ChannelAffiliate},
		{"email with webmail referrer beats referral", Input{URL: page, Referrer: "https://mail.example.org/inbox", UTMMedium: "email"}, ChannelEmail},
		// 11: Referral
		{"referral", Input{URL: page, Referrer: "https://blog.dogfood.example.org/review"}, ChannelReferral},
		{"android-app referral", Input{URL: page, Referrer: "android-app://com.google.android.gm"}, ChannelReferral},
		{"notfacebook.com is referral not social", Input{URL: page, Referrer: "https://notfacebook.com/x"}, ChannelReferral},
		// 12: Unassigned
		{"utm campaign only", Input{URL: page, UTMCampaign: "summer-sale"}, ChannelUnassigned},
		{"utm term only via url", Input{URL: page + "?utm_term=dog+food"}, ChannelUnassigned},
		{"unrecognized source+medium", Input{URL: page, UTMSource: "mystery", UTMMedium: "carrier-pigeon"}, ChannelUnassigned},
		// 12, via an unresolvable referrer. url.Parse reads a schemeless string
		// as a path, so these yield no host and no error; booking them Direct
		// would hide real referred traffic inside the one bucket nobody
		// inspects. Unassigned is visible instead.
		{"schemeless referrer is unassigned not direct", Input{URL: page, Referrer: "www.google.com/search?q=x"}, ChannelUnassigned},
		{"bare host referrer is unassigned not direct", Input{URL: page, Referrer: "google.com"}, ChannelUnassigned},
		{"unparseable referrer is unassigned not direct", Input{URL: page, Referrer: "://bad"}, ChannelUnassigned},
		{"garbage referrer is unassigned not direct", Input{URL: page, Referrer: "not a url"}, ChannelUnassigned},
		// An unresolvable referrer is only a fallback: a real UTM still wins.
		{"utm beats unresolvable referrer", Input{URL: page, Referrer: "google.com", UTMSource: "google", UTMMedium: "cpc"}, ChannelPaidSearch},
		// 13: Direct
		{"no referrer no utm", Input{URL: page}, ChannelDirect},
		// A self-referral is RESOLVED and deliberately blanked, so it must stay
		// Direct — it is not an unclassifiable signal.
		{"self-referral collapses to direct", Input{URL: page, Referrer: "https://pugandpals.example.com/"}, ChannelDirect},
		{"self-referral with www collapses to direct", Input{URL: page, Referrer: "https://www.pugandpals.example.com/"}, ChannelDirect},
		// Precedence: search ref + email medium → Organic Search (rule 6 before 9).
		{"search ref beats email medium", Input{URL: page, Referrer: "https://www.google.com", UTMMedium: "email"}, ChannelOrganicSearch},
		// Precedence: social ref + affiliate medium → Organic Social (7 before 10).
		{"social ref beats affiliate medium", Input{URL: page, Referrer: "https://reddit.com/x", UTMMedium: "affiliate"}, ChannelOrganicSocial},
		// Case-insensitivity of src/med.
		{"uppercase medium classified", Input{URL: page, UTMSource: "Google", UTMMedium: "CPC"}, ChannelPaidSearch},
		// UTM completed from the landing URL feeds classification.
		{"utm from url query classifies", Input{URL: page + "?utm_source=google&utm_medium=cpc"}, ChannelPaidSearch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if out := Derive(c.in); out.Channel != c.want {
				t.Errorf("Channel = %q, want %q (in: %+v)", out.Channel, c.want, c.in)
			}
		})
	}
}

func TestIsPaidMedium(t *testing.T) {
	paid := []string{"cpc", "cpm", "cpv", "cpa", "ecpc", "ppc", "retargeting", "paid", "paid_social", "paid-search", "paidsearch"}
	for _, m := range paid {
		if !isPaidMedium(m) {
			t.Errorf("isPaidMedium(%q) = false, want true", m)
		}
	}
	organic := []string{"", "organic", "social", "email", "referral", "affiliate", "display", "video"}
	for _, m := range organic {
		if isPaidMedium(m) {
			t.Errorf("isPaidMedium(%q) = true, want false", m)
		}
	}
}

func TestIsGoogleHost(t *testing.T) {
	yes := []string{"google.com", "google.co.uk", "google.de", "images.google.co.in", "news.google.com", "google"}
	for _, h := range yes {
		if !isGoogleHost(h) {
			t.Errorf("isGoogleHost(%q) = false, want true", h)
		}
	}
	no := []string{"", "googleusercontent.com", "notgoogle.com", "agoogle.de", "example.com"}
	for _, h := range no {
		if isGoogleHost(h) {
			t.Errorf("isGoogleHost(%q) = true, want false", h)
		}
	}
}
