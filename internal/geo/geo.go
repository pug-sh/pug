package geo

import (
	"net/http"
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
func ClientIP(h http.Header) string {
	if ip := h.Get(HeaderCFConnectingIP); ip != "" {
		return ip
	}
	if ip := h.Get(HeaderTrueClientIP); ip != "" {
		return ip
	}
	if xff := h.Get(HeaderXForwardedFor); xff != "" {
		// X-Forwarded-For can be comma-separated; first entry is the client.
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	return ""
}

// Provider resolves geo data from HTTP request headers.
//
// Implementations can read from proxy-injected headers (e.g. Cloudflare) or
// perform a local MaxMind MMDB lookup once the IP is extracted from the headers.
// Returned Location should only contain non-empty values.
type Provider interface {
	Locate(h http.Header) Location
}
