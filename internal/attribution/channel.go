package attribution

import (
	"slices"
	"strings"
)

// This file is the single normative marketing-channel taxonomy
// (docs/architecture/web-analytics.md). Every channel a pug event can carry is
// classified here — profiles.md's "no ad hoc channel" rule points at this
// derivation. Changing a rule, a set entry, or the rule ORDER changes what
// lands in the permanent rollups; treat edits as taxonomy changes, not
// refactors, and pin them in channel_test.go.

// Channel values emitted by classifyChannel.
const (
	ChannelPaidSearch    = "Paid Search"
	ChannelPaidSocial    = "Paid Social"
	ChannelPaidVideo     = "Paid Video"
	ChannelDisplay       = "Display"
	ChannelPaidOther     = "Paid Other"
	ChannelOrganicSearch = "Organic Search"
	ChannelOrganicSocial = "Organic Social"
	ChannelOrganicVideo  = "Organic Video"
	ChannelEmail         = "Email"
	ChannelAffiliate     = "Affiliate"
	ChannelReferral      = "Referral"
	ChannelUnassigned    = "Unassigned"
	ChannelDirect        = "Direct"
)

// Domain sets are matched against the referrer domain after referrerDomain's
// blanks (self-referral, authIntermediaryHosts) by dot-boundary suffix
// ("l.facebook.com" matches "facebook.com"). Source sets are matched against
// the lowercased utm_source verbatim, plus the same domain suffix match for
// utm_source values that are themselves domains.
// Google's ccTLD family (google.com, google.co.uk, images.google.de, …) is
// matched structurally by isGoogleSearchHost instead of enumeration.
var (
	searchDomains = []string{
		"bing.com", "duckduckgo.com", "yahoo.com", "yahoo.co.jp", "baidu.com",
		"yandex.com", "yandex.ru", "ecosia.org", "search.brave.com",
		"startpage.com", "perplexity.ai", "qwant.com", "kagi.com",
	}
	searchSources = []string{
		"google", "bing", "duckduckgo", "ddg", "yahoo", "baidu", "yandex",
		"ecosia", "brave", "startpage", "perplexity", "qwant", "kagi",
	}

	socialDomains = []string{
		"facebook.com", "fb.com", "fb.me", "messenger.com", "instagram.com",
		"twitter.com", "x.com", "t.co", "linkedin.com", "lnkd.in",
		"tiktok.com", "pinterest.com", "pin.it", "reddit.com", "redd.it",
		"threads.net", "bsky.app", "news.ycombinator.com", "mastodon.social",
		"whatsapp.com", "telegram.org", "t.me",
	}
	socialSources = []string{
		"facebook", "fb", "instagram", "ig", "twitter", "x", "linkedin",
		"tiktok", "pinterest", "reddit", "threads", "bluesky", "bsky",
		"hackernews", "hn", "whatsapp", "telegram", "mastodon",
	}

	videoDomains = []string{"youtube.com", "youtu.be", "vimeo.com", "twitch.tv", "dailymotion.com"}
	videoSources = []string{"youtube", "yt", "vimeo", "twitch", "dailymotion"}

	displayMediums = []string{"display", "banner", "expandable", "interstitial", "cpm"}
	socialMediums  = []string{"social", "social-network", "social-media", "sm"}
	emailTokens    = []string{"email", "e-mail", "e_mail", "newsletter"}

	// googleSearchSubdomains are the labels that may precede "google.<tld>" on a
	// search surface; "" is the apex. An allow-list, so an unlisted subdomain
	// (accounts, mail, search — and whatever google ships next) books a visible
	// Referral instead of silently re-inflating Organic Search. gemini and lens
	// are here because perplexity.ai is a searchDomain: same kind of surface.
	googleSearchSubdomains = []string{
		"", "www", "m", "images", "news", "maps", "scholar", "books", "video", "cse",
		"gemini", "lens",
	}

	// webmailDomains are webmail clients: a click there is a click inside an
	// email, so it books Email. The mail.yahoo/mail.yandex hosts also suffix-match
	// a search domain — one entry per such sibling, or the Organic Search defect
	// survives on the host left out.
	webmailDomains = []string{
		"mail.google.com", "mail.yahoo.com", "mail.yahoo.co.jp", "mail.yandex.ru",
		"mail.yandex.com", "mail.aol.com", "outlook.live.com", "outlook.office.com",
		"outlook.office365.com", "mail.proton.me", "mail.zoho.com",
	}
)

// classifyChannel implements the normative rule table, first match wins.
// ref is referrerDomain's output (lowercased; self-referrals and
// authIntermediaryHosts already blanked); src/med are the EFFECTIVE
// utm_source/utm_medium (after URL completion) and are lowercased here;
// anySignal reports that the event carried SOME attribution signal — any of
// the five effective UTM values, or a referrer that was sent but yielded no
// host (referrerDomain's unresolved).
//
// Rule 12 is what keeps an unclassifiable signal OUT of Direct: an
// unresolvable referrer books as Unassigned, which is visible on a dashboard,
// rather than hiding inside the Direct bucket that is expected to be large.
// A self-referral or an authIntermediaryHosts bounce is deliberately NOT a
// signal — both blank to Direct via 13.
//
//	 # | rule                                                  | channel
//	 1 | paid medium AND search source/ref                     | Paid Search
//	 2 | paid medium AND social source/ref                     | Paid Social
//	 3 | paid medium AND video source/ref                      | Paid Video
//	 4 | medium ∈ {display, banner, expandable, interstitial,  | Display
//	   |           cpm}                                        |
//	 5 | paid medium, unmatched source                         | Paid Other
//	 6 | search source/ref, or medium = organic                | Organic Search
//	 7 | social source/ref, or medium ∈ {social,               | Organic Social
//	   |   social-network, social-media, sm}                   |
//	 8 | video source/ref, or medium = video                   | Organic Video
//	 9 | source or medium ∈ {email, e-mail, e_mail, newsletter}| Email
//	   |   or a webmail referrer                               |
//	10 | medium = affiliate                                    | Affiliate
//	11 | ref != ""                                             | Referral
//	12 | any UTM, or an unresolvable referrer, but             | Unassigned
//	   |   unclassifiable                                      |
//	13 | otherwise                                             | Direct
func classifyChannel(ref, src, med string, anySignal bool) string {
	src = strings.ToLower(src)
	med = strings.ToLower(med)

	// A webmail ref is an email click, not the provider's search engine:
	// mail.yahoo.com would otherwise suffix-match yahoo.com. Guards the ref arms
	// only — a search utm_source still wins.
	webmail := matchesHost(ref, webmailDomains)
	search := matchesSource(src, searchSources, searchDomains) || isGoogleSearchHost(src) ||
		(!webmail && (matchesHost(ref, searchDomains) || isGoogleSearchHost(ref)))
	social := matchesSource(src, socialSources, socialDomains) || matchesHost(ref, socialDomains)
	video := matchesSource(src, videoSources, videoDomains) || matchesHost(ref, videoDomains)
	paid := isPaidMedium(med)

	switch {
	case paid && search:
		return ChannelPaidSearch
	case paid && social:
		return ChannelPaidSocial
	case paid && video:
		return ChannelPaidVideo
	case slices.Contains(displayMediums, med):
		return ChannelDisplay
	case paid:
		return ChannelPaidOther
	case search || med == "organic":
		return ChannelOrganicSearch
	case social || slices.Contains(socialMediums, med):
		return ChannelOrganicSocial
	case video || med == "video":
		return ChannelOrganicVideo
	case webmail || slices.Contains(emailTokens, src) || slices.Contains(emailTokens, med):
		return ChannelEmail
	case med == "affiliate":
		return ChannelAffiliate
	case ref != "":
		return ChannelReferral
	case anySignal:
		return ChannelUnassigned
	default:
		return ChannelDirect
	}
}

// isPaidMedium implements the paid-medium pattern ^(.*cp.*|ppc|retargeting|paid.*)$
// without regexp: any medium containing "cp" (cpc, cpm, ecpc, …), exactly
// "ppc"/"retargeting", or a "paid" prefix (paid, paid_social, paid-search, …).
func isPaidMedium(med string) bool {
	if med == "" {
		return false
	}
	return strings.Contains(med, "cp") || med == "ppc" || med == "retargeting" || strings.HasPrefix(med, "paid")
}

// matchesHost reports whether host equals a set entry or is a subdomain of one
// (dot-boundary suffix match, so "l.facebook.com" matches "facebook.com" but
// "notfacebook.com" does not).
func matchesHost(host string, domains []string) bool {
	if host == "" {
		return false
	}
	for _, d := range domains {
		// Suffix first, then check the boundary in place. The obvious
		// HasSuffix(host, "."+d) concatenates once per set entry per event,
		// and these sets are scanned ~50 deep for every referred pageview.
		if !strings.HasSuffix(host, d) {
			continue
		}
		if len(host) == len(d) || host[len(host)-len(d)-1] == '.' {
			return true
		}
	}
	return false
}

// matchesSource matches a utm_source value: verbatim against the bare-name
// set, or as a domain against the domain set (some tools stamp utm_source
// with the full referrer domain, e.g. utm_source=facebook.com).
func matchesSource(src string, names, domains []string) bool {
	return slices.Contains(names, src) || matchesHost(src, domains)
}

// isGoogleSearchHost matches google's SEARCH hosts without enumerating the
// ccTLDs: a dot-boundary suffix of the host must be "google." followed by a
// TLD-shaped tail (one or two labels of 2–3 alphabetic chars), AND the whole
// prefix before it must be a googleSearchSubdomains entry —
// google.com, google.co.uk and images.google.de match; accounts.google.com,
// mail.google.com and search.google.com do not. The tail-shape check is what
// keeps reverse-DNS app hosts out: android-app://com.google.android.gm
// contains the suffix "google.android.gm", whose tail ("android.gm") is not
// TLD-shaped, so it stays a Referral per the taxonomy. "googleusercontent.com"
// never matches (no "google." at a label boundary).
func isGoogleSearchHost(host string) bool {
	if host == "google" { // domain-ish bare "google" in utm_source
		return true
	}
	// Walk label boundaries to the first "google.<tld>": everything walked past
	// is the subdomain — ALL of it, not the last label, or evil.www.google.com
	// would read as "www" and pass.
	for i := 0; ; {
		if tail, ok := strings.CutPrefix(host[i:], "google."); ok && tldShaped(tail) {
			sub := strings.TrimSuffix(host[:i], ".") // i is past the boundary dot; "" at the apex
			return slices.Contains(googleSearchSubdomains, sub)
		}
		dot := strings.IndexByte(host[i:], '.')
		if dot < 0 {
			return false
		}
		i += dot + 1
	}
}

// tldShaped reports whether tail looks like a public-suffix tail of google's
// ccTLD family: one or two labels, each 2–3 lowercase-alphabetic characters
// ("com", "de", "co.uk", "com.br"). Cut rather than Split: this runs for every
// google-referred pageview, and Split's slice would be the only allocation
// classifyChannel makes.
func tldShaped(tail string) bool {
	first, rest, more := strings.Cut(tail, ".")
	if !tldLabel(first) {
		return false
	}
	if !more {
		return true
	}
	// A second dot means a third label — too long to be a public-suffix tail.
	return !strings.Contains(rest, ".") && tldLabel(rest)
}

func tldLabel(l string) bool {
	if len(l) < 2 || len(l) > 3 {
		return false
	}
	for i := range len(l) {
		if l[i] < 'a' || l[i] > 'z' {
			return false
		}
	}
	return true
}
