package geo

import (
	"net/http"
	"net/netip"
	"strings"
)

// Auto-property keys used by geo providers.
const (
	// PropIP is the canonical visitor-IP key. The IP is personal data and is
	// never persisted: the SDK ingestion handlers strip it before publishing —
	// events in enrichGeo, identify traits in Identify — so it cannot reach NATS
	// or ClickHouse, and providers must not emit it in their Location. The strip
	// targets this canonical key (the only one our enrichment and SDKs produce).
	// Kept only so the strip logic and IP-lookup providers can refer to it.
	PropIP         = "$ip"
	PropContinent  = "$continent"
	PropCountry    = "$country"
	PropRegion     = "$region"
	PropCity       = "$city"
	PropPostalCode = "$postalCode"
	PropMetroCode  = "$metroCode"
	PropLatitude   = "$latitude"
	PropLongitude  = "$longitude"
	PropTimezone   = "$timezone"
)

// Location is a set of geo properties resolved for a request.
// Keys are auto-property names (e.g. "$country", "$city").
// Providers decide which keys to populate — different providers may
// return different sets of keys.
type Location map[string]string

// Common IP headers, ordered by trust level.
const (
	HeaderCFConnectingIP = "CF-Connecting-IP"
	HeaderTrueClientIP   = "True-Client-IP"
	HeaderXForwardedFor  = "X-Forwarded-For"
)

// ClientIP extracts the visitor's IP address from request headers.
// Tries CF-Connecting-IP (Cloudflare), then True-Client-IP (CDNs/LBs),
// then the first address in X-Forwarded-For.
//
// No Provider calls it: the Cloudflare provider resolves geo from CF-* headers
// and never reads the IP. Its one production caller is cookieless identity
// resolution (resolveCookieless), which feeds the IP into an HMAC and never
// attaches it to an event. It remains the extraction primitive a future
// IP-lookup Provider (see Provider) would use — such a provider must hash/use
// the IP transiently and keep it out of the returned Location, as the raw IP
// must never be persisted (see PropIP).
//
// Every candidate is parsed and re-serialised, and anything unparseable is
// skipped rather than returned verbatim. Two reasons, both from the HMAC caller:
//
//   - Framing. The hash is project ‖ 0x00 ‖ ip ‖ 0x00 ‖ ua, which is injective
//     only while no field before the last can contain the separator. Parsing is
//     what makes that true of ip by construction instead of by accident — Go's
//     HTTP/1.1 and HTTP/2 header validation happen to reject NUL today, so the
//     property rested on the transport rather than on this function.
//   - Canonical form. "2001:0DB8::0001" and "2001:db8::1" are one address; left
//     as written they hash to two different visitors.
//
// NOTE: these headers are still CLIENT-SUPPLIED and are not authenticated here.
// Validation makes the value well-formed, not trustworthy — an origin reachable
// outside the CDN can be fed any address. Restricting who may set them belongs
// at the edge (a trusted-proxy allowlist), not in this function.
func ClientIP(h http.Header) string {
	for _, header := range []string{HeaderCFConnectingIP, HeaderTrueClientIP} {
		if ip, ok := parseClientIP(h.Get(header)); ok {
			return ip
		}
	}
	if xff := h.Get(HeaderXForwardedFor); xff != "" {
		// X-Forwarded-For can be comma-separated; first entry is the client.
		first, _, _ := strings.Cut(xff, ",")
		if ip, ok := parseClientIP(first); ok {
			return ip
		}
	}
	return ""
}

// parseClientIP validates one header value and returns it in canonical form.
// The zone on a link-local address ("fe80::1%eth0") is dropped: it names a local
// interface, not the visitor, and would otherwise split one address into as many
// identities as there are zone spellings.
func parseClientIP(raw string) (string, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	return addr.WithZone("").String(), true
}

// Provider resolves geo data from HTTP request headers.
//
// Implementations can read from proxy-injected headers (e.g. Cloudflare) or
// perform a local MaxMind MMDB lookup once the IP is extracted from the headers.
// Returned Location should only contain non-empty values.
type Provider interface {
	Locate(h http.Header) Location
}
