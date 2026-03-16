package geo

import "net/http"

// Auto-property keys written by geo providers.
const (
	PropCountry = "$country"
	PropRegion  = "$region"
	PropCity    = "$city"
)

// Location holds geo data resolved for a request.
type Location struct {
	Country string
	Region  string
	City    string
}

// IsZero reports whether no location data was resolved.
func (l *Location) IsZero() bool {
	return l.Country == "" && l.Region == "" && l.City == ""
}

// Provider resolves geo data from HTTP request headers.
//
// Implementations can read from proxy-injected headers (e.g. Cloudflare) or
// perform a local MaxMind MMDB lookup once the IP is extracted from the headers.
type Provider interface {
	Locate(h http.Header) *Location
}
