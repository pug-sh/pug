// Package attribution derives web-analytics auto-properties from the
// navigation properties a client sends: URL decomposition ($pathname,
// $hostname), referrer domain with self-referral blanking ($referrerDomain),
// UTM completion from the URL query string, screen-size formatting
// ($screenSize), locale casing normalization ($locale), and marketing-channel
// classification ($channel — see channel.go for the normative taxonomy).
//
// The package is a stdlib-only leaf (net/url, strings, strconv) so the SDK
// ingest handler, internal/core/clickhouse, and the demo seeder can all import
// it — demo data and production traffic classify identically because they run
// the same Derive.
//
// Derive is pure: it never mutates its input and consults no providers. Which
// derived value wins over a client-sent one (the per-key overwrite policy) is
// expressed by what the caller feeds into Input: derive-if-absent keys pass
// the client value through Input and Derive echoes it back when non-empty;
// always-server-derived keys ($referrerDomain, $channel) have no Input field
// at all, so a client value cannot influence them and callers must strip them
// from client input before persisting Output.
package attribution

import (
	"net/url"
	"strconv"
	"strings"
)

// Canonical auto-property keys owned by this package. Client-sent navigation
// keys ($url, $referrer, UTM ×5, $locale, $pageTitle) and every key Derive
// computes. Other packages must reference these instead of string literals.
const (
	PropURL            = "$url"
	PropReferrer       = "$referrer"
	PropReferrerDomain = "$referrerDomain"
	PropChannel        = "$channel"
	PropPathname       = "$pathname"
	PropHostname       = "$hostname"
	PropLocale         = "$locale"
	PropScreenSize     = "$screenSize"
	PropPageTitle      = "$pageTitle"
	PropUTMSource      = "$utmSource"
	PropUTMMedium      = "$utmMedium"
	PropUTMCampaign    = "$utmCampaign"
	PropUTMTerm        = "$utmTerm"
	PropUTMContent     = "$utmContent"
)

// Input carries the client-sent values Derive works from. Empty string / zero
// means "absent". Pathname, Hostname, and ScreenSize are the client values for
// the derive-if-absent keys (an SDK may send logical route paths); when
// non-empty they win and Derive echoes them into Output unchanged.
type Input struct {
	URL      string
	Referrer string

	// Client-sent values for derive-if-absent keys.
	Pathname   string
	Hostname   string
	ScreenSize string

	// Client-sent UTM values; completed from the URL query only when absent.
	UTMSource   string
	UTMMedium   string
	UTMCampaign string
	UTMTerm     string
	UTMContent  string

	Locale       string
	ScreenWidth  int64
	ScreenHeight int64
}

// ServerOnlyKeys are the keys Derive always computes itself. They have no
// Input field, so a client-sent copy cannot influence derivation — but it
// would survive untouched into storage, because the write side never clears a
// key. Callers must delete these from client input before applying Output,
// mirroring the bot enrichers' strip.
var ServerOnlyKeys = [...]string{PropReferrerDomain, PropChannel}

// Source reads client-sent auto-properties out of whatever map shape a caller
// holds.
type Source interface {
	// String returns the client value for one of this package's Prop* keys,
	// coerced the way the promotion layer would store it (mirroring
	// clickhouse.SplitPromotedAutoProperties). Empty means absent.
	String(key string) string
	// ScreenDims returns the $screenWidth/$screenHeight numeric slots, zero
	// when absent. The caller reads them rather than Source naming the keys:
	// they belong to internal/autoprop, which this package deliberately does
	// not import so it can stay a stdlib-only leaf.
	ScreenDims() (width, height int64)
}

// InputFrom assembles an Input by reading every key Derive consults. The SDK
// ingest handler and the demo seeder both go through it so neither can drift
// into feeding Derive a different set of inputs than the other — the promise
// that demo data and production traffic classify identically is only worth
// anything if the inputs match, not just the derivation.
func InputFrom(src Source) Input {
	w, h := src.ScreenDims()
	return Input{
		URL:          src.String(PropURL),
		Referrer:     src.String(PropReferrer),
		Pathname:     src.String(PropPathname),
		Hostname:     src.String(PropHostname),
		ScreenSize:   src.String(PropScreenSize),
		UTMSource:    src.String(PropUTMSource),
		UTMMedium:    src.String(PropUTMMedium),
		UTMCampaign:  src.String(PropUTMCampaign),
		UTMTerm:      src.String(PropUTMTerm),
		UTMContent:   src.String(PropUTMContent),
		Locale:       src.String(PropLocale),
		ScreenWidth:  w,
		ScreenHeight: h,
	}
}

// Pairs returns every derived value under its canonical auto-property key, in
// a fixed order. This is the whole write side of the enrichment: Derive has
// already resolved the per-key overwrite policy (echoing a winning client
// value back unchanged), so a caller writes each non-empty value and is done.
// Returned as an array rather than a map so the hot ingest path pays no
// allocation, and so adding a derived key is one edit rather than one per
// caller.
func (o Output) Pairs() [11]struct{ Key, Value string } {
	return [11]struct{ Key, Value string }{
		{PropPathname, o.Pathname},
		{PropHostname, o.Hostname},
		{PropReferrerDomain, o.ReferrerDomain},
		{PropChannel, o.Channel},
		{PropScreenSize, o.ScreenSize},
		{PropUTMSource, o.UTMSource},
		{PropUTMMedium, o.UTMMedium},
		{PropUTMCampaign, o.UTMCampaign},
		{PropUTMTerm, o.UTMTerm},
		{PropUTMContent, o.UTMContent},
		{PropLocale, o.Locale},
	}
}

// Output holds the effective values after derivation. Empty string means "no
// value" — for promoted columns that is exactly what gets stored, so callers
// can skip writing empty outputs.
type Output struct {
	Pathname       string
	Hostname       string
	ReferrerDomain string
	Channel        string
	ScreenSize     string
	UTMSource      string
	UTMMedium      string
	UTMCampaign    string
	UTMTerm        string
	UTMContent     string
	Locale         string
}

// Derive computes the effective web-analytics properties for one event.
//
// URL rules: $pathname/$hostname/UTM completion derive only from a parseable
// http(s) URL with a non-empty host — garbage input fabricates nothing (no
// synthetic "/"). $channel likewise derives only when such a URL is present,
// so non-web events (no $url) carry no channel rather than a misleading
// "Direct". The referrer accepts any scheme with a host (android-app://…
// classifies as Referral); a referrer that was sent but resolves to no host
// classifies as Unassigned — never Direct, which would hide it in the one
// bucket that is expected to be large. $url itself is never mutated, and the
// path is not normalized (case/trailing-slash normalization is lossy).
func Derive(in Input) Output {
	var out Output

	pageURL, pageHost := parsePageURL(in.URL)

	out.Pathname = in.Pathname
	if out.Pathname == "" && pageURL != nil {
		// Path, not EscapedPath: the decoded form is the one migration 008's
		// SQL mirror can reproduce (decodeURLComponent(path(url))). Go's
		// escaping rules for a path — preserving sub-delims and ":@" while
		// percent-encoding non-ASCII — have no ClickHouse equivalent, so
		// EscapedPath would store "/caf%C3%A9" live against the mirror's
		// "/café" and split one page into two permanent rollup dim_values.
		// The cost is that an encoded "%2F" stops being distinguishable from
		// a real separator; encoded slashes in paths are rare and the
		// symmetry is worth more.
		out.Pathname = pageURL.Path
		if out.Pathname == "" {
			out.Pathname = "/"
		}
	}

	out.Hostname = in.Hostname
	if out.Hostname == "" {
		out.Hostname = pageHost
	}

	out.UTMSource = in.UTMSource
	out.UTMMedium = in.UTMMedium
	out.UTMCampaign = in.UTMCampaign
	out.UTMTerm = in.UTMTerm
	out.UTMContent = in.UTMContent
	// Query() parses and allocates every parameter — gclid, session tokens,
	// the lot — to read five keys, so it is skipped when there is nothing to
	// read or nothing left to fill. The common pageview has neither: no query
	// string at all, or an SDK that already sent its UTMs.
	if pageURL != nil && pageURL.RawQuery != "" && (out.UTMSource == "" || out.UTMMedium == "" ||
		out.UTMCampaign == "" || out.UTMTerm == "" || out.UTMContent == "") {
		q := pageURL.Query()
		completeUTM(&out.UTMSource, q, "utm_source")
		completeUTM(&out.UTMMedium, q, "utm_medium")
		completeUTM(&out.UTMCampaign, q, "utm_campaign")
		completeUTM(&out.UTMTerm, q, "utm_term")
		completeUTM(&out.UTMContent, q, "utm_content")
	}

	// Self-referral compares against the URL's host, NOT out.Hostname: the
	// latter echoes a client-sent $hostname, which would let a client steer
	// the server-only $referrerDomain/$channel — sending a $hostname that
	// disagrees with $url defeats blanking and fills the Referrers panel with
	// the tenant's own domain, and sending the referrer's own host suppresses
	// a real acquisition channel as Direct. The client value is still the
	// stored $hostname (derive-if-absent, so SDKs keep logical hostnames); it
	// only stops deciding the rule. With no URL there is no server-known host,
	// so the client's is the only signal available and self-blanking still
	// works off it — $channel is gated on pageURL anyway, so nothing
	// classifiable depends on it.
	ownHost := pageHost
	if pageURL == nil {
		ownHost = in.Hostname
	}
	var refUnresolved bool
	out.ReferrerDomain, refUnresolved = referrerDomain(in.Referrer, ownHost)

	if pageURL != nil {
		anyUTM := out.UTMSource != "" || out.UTMMedium != "" || out.UTMCampaign != "" ||
			out.UTMTerm != "" || out.UTMContent != ""
		out.Channel = classifyChannel(out.ReferrerDomain, out.UTMSource, out.UTMMedium, anyUTM || refUnresolved)
	}

	out.ScreenSize = in.ScreenSize
	if out.ScreenSize == "" && in.ScreenWidth > 0 && in.ScreenHeight > 0 {
		out.ScreenSize = strconv.FormatInt(in.ScreenWidth, 10) + "x" + strconv.FormatInt(in.ScreenHeight, 10)
	}

	out.Locale = NormalizeLocale(in.Locale)

	return out
}

// parsePageURL returns the parsed page URL and its lowercased host when raw is
// a parseable http(s) URL with a non-empty host, else (nil, ""). This is the
// validity gate for every URL-derived output. The host comes back with it
// because every caller wants it lowercased and Hostname() rescans u.Host.
func parsePageURL(raw string) (*url.URL, string) {
	if raw == "" {
		return nil, ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, ""
	}
	// No ToLower: url.Parse already lowercases Scheme. It does NOT lowercase
	// the host, which is why the one below is real.
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, ""
	}
	host := u.Hostname()
	if host == "" {
		return nil, ""
	}
	return u, strings.ToLower(host)
}

func completeUTM(dst *string, q url.Values, param string) {
	if *dst == "" {
		*dst = q.Get(param)
	}
}

// referrerDomain extracts the referrer's host — any scheme qualifies as long
// as a host is present — lowercased, with one leading "www." stripped, and
// blanked when it equals the event's own hostname after the same "www." strip
// (a self-referral). Subdomains are deliberately NOT collapsed to a
// registrable domain: app.example.com referred from www.example.com stays a
// referral in v1 (no publicsuffix dependency).
//
// unresolved reports a referrer that WAS sent but yielded no host, which the
// empty domain alone cannot express. url.Parse reads a schemeless string as a
// path, so "www.google.com/search" and a bare "google.com" both parse without
// error into no host — indistinguishable from "no referrer at all" unless it
// is flagged here, and silently booking real referred traffic as Direct is
// invisible precisely because Direct is expected to be large. A self-referral
// is resolved, NOT unresolved: its blanking is deliberate and must keep
// classifying as Direct.
func referrerDomain(referrer, ownHostname string) (dom string, unresolved bool) {
	if referrer == "" {
		return "", false
	}
	u, err := url.Parse(referrer)
	if err != nil {
		return "", true
	}
	dom = stripOneWWW(strings.ToLower(u.Hostname()))
	if dom == "" {
		return "", true
	}
	if own := stripOneWWW(strings.ToLower(ownHostname)); own != "" && dom == own {
		return "", false
	}
	return dom, false
}

// stripOneWWW removes exactly one leading "www." label; "www.www.x.com" keeps
// its second www so distinct hosts stay distinct.
func stripOneWWW(host string) string {
	return strings.TrimPrefix(host, "www.")
}

// NormalizeLocale canonicalizes a BCP-47-ish tag's casing and separator
// ("en-us" → "en-US", "zh_hans_cn" → "zh-Hans-CN") so language dimensions
// don't fragment on case variants — rollup rows are permanent, so this must
// happen before storage. Re-casing follows the BCP 47 convention by subtag
// length (2-letter region upper, 4-letter script title, everything else
// lower); subtags are never reordered, validated, or dropped. Numeric region
// subtags ("es-419") pass through untouched by casing.
func NormalizeLocale(locale string) string {
	locale = strings.TrimSpace(locale)
	if locale == "" {
		return ""
	}
	parts := strings.Split(strings.ReplaceAll(locale, "_", "-"), "-")
	for i, p := range parts {
		switch {
		case i == 0:
			parts[i] = strings.ToLower(p)
		case len(p) == 2:
			parts[i] = strings.ToUpper(p)
		case len(p) == 4:
			parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
		default:
			parts[i] = strings.ToLower(p)
		}
	}
	return strings.Join(parts, "-")
}
