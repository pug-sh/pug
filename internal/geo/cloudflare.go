package geo

import "net/http"

// Cloudflare geo headers.
// CF-IPCountry is added when IP Geolocation is enabled.
// CF-Region, CF-IPCity, and other location headers require the
// "Add visitor location headers" Managed Transform.
// https://developers.cloudflare.com/rules/transform/managed-transforms/reference/
const (
	cfHeaderContinent  = "CF-IPContinent"
	cfHeaderCountry    = "CF-IPCountry"
	cfHeaderRegion     = "CF-Region"
	cfHeaderCity       = "CF-IPCity"
	cfHeaderPostalCode = "CF-Postal-Code"
	cfHeaderMetroCode  = "CF-Metro-Code"
	cfHeaderLatitude   = "CF-IPLatitude"
	cfHeaderLongitude  = "CF-IPLongitude"
	cfHeaderTimezone   = "CF-Timezone"
)

// CloudflareProvider resolves geo data from Cloudflare proxy headers.
type CloudflareProvider struct{}

func (CloudflareProvider) Locate(h http.Header) Location {
	loc := Location{}

	if ip := ClientIP(h); ip != "" {
		loc[PropIP] = ip
	}
	if v := h.Get(cfHeaderContinent); v != "" {
		loc[PropContinent] = v
	}
	// Cloudflare returns "XX" when country is unknown and "T1" for Tor exit nodes.
	if v := h.Get(cfHeaderCountry); v != "" && v != "XX" && v != "T1" {
		loc[PropCountry] = v
	}
	// Cloudflare returns "Unknown" for region/city when location is indeterminate.
	if v := h.Get(cfHeaderRegion); v != "" && v != "Unknown" {
		loc[PropRegion] = v
	}
	if v := h.Get(cfHeaderCity); v != "" && v != "Unknown" {
		loc[PropCity] = v
	}
	if v := h.Get(cfHeaderPostalCode); v != "" {
		loc[PropPostalCode] = v
	}
	if v := h.Get(cfHeaderMetroCode); v != "" {
		loc[PropMetroCode] = v
	}
	if v := h.Get(cfHeaderLatitude); v != "" {
		loc[PropLatitude] = v
	}
	if v := h.Get(cfHeaderLongitude); v != "" {
		loc[PropLongitude] = v
	}
	if v := h.Get(cfHeaderTimezone); v != "" {
		loc[PropTimezone] = v
	}

	return loc
}
