package geo

import "net/http"

// Cloudflare proxy headers injected on every request.
// https://developers.cloudflare.com/fundamentals/reference/http-request-headers/
const (
	cfHeaderCountry = "CF-IPCountry"
	cfHeaderRegion  = "CF-Region"
	cfHeaderCity    = "CF-City"
)

// CloudflareProvider resolves geo data from Cloudflare proxy headers.
// To switch to a local MMDB lookup, implement a new Provider that reads
// CF-Connecting-IP and queries the database file.
type CloudflareProvider struct{}

func (CloudflareProvider) Locate(h http.Header) *Location {
	return &Location{
		Country: h.Get(cfHeaderCountry),
		Region:  h.Get(cfHeaderRegion),
		City:    h.Get(cfHeaderCity),
	}
}
