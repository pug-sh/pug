package geo

import (
	"net/http"
	"strings"
)

// Auto-property keys used by geo providers.
const (
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
