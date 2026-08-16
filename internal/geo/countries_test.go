package geo

import "testing"

func TestIsCountryCode(t *testing.T) {
	for _, code := range []string{"US", "GB", "IN", "XK", "GS", "UM"} {
		if !IsCountryCode(code) {
			t.Errorf("IsCountryCode(%q) = false, want true", code)
		}
	}
	// ZZ and G1 are correctly shaped but unassigned; XX and T1 are Cloudflare's
	// unknown/Tor sentinels, already dropped at the header but never a country.
	for _, code := range []string{"", "us", "USA", "unknown", "ZZ", "G1", "XX", "T1"} {
		if IsCountryCode(code) {
			t.Errorf("IsCountryCode(%q) = true, want false", code)
		}
	}
}

func TestCountryCodeSetSize(t *testing.T) {
	// 249 officially assigned ISO 3166-1 alpha-2 codes, plus XK for Kosovo.
	if len(countrySet) != 250 {
		t.Errorf("country set has %d codes, want 250", len(countrySet))
	}
}
