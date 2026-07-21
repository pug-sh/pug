package geo

import (
	"net/http"
	"testing"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		want    string
	}{
		{
			"CF-Connecting-IP takes priority",
			http.Header{
				"Cf-Connecting-Ip": {"1.1.1.1"},
				"True-Client-Ip":   {"2.2.2.2"},
				"X-Forwarded-For":  {"3.3.3.3"},
			},
			"1.1.1.1",
		},
		{
			"True-Client-IP fallback",
			http.Header{
				"True-Client-Ip":  {"2.2.2.2"},
				"X-Forwarded-For": {"3.3.3.3"},
			},
			"2.2.2.2",
		},
		{
			"X-Forwarded-For fallback",
			http.Header{"X-Forwarded-For": {"3.3.3.3"}},
			"3.3.3.3",
		},
		{
			"X-Forwarded-For comma-separated — takes first",
			http.Header{"X-Forwarded-For": {"3.3.3.3, 4.4.4.4, 5.5.5.5"}},
			"3.3.3.3",
		},
		{
			"X-Forwarded-For with spaces",
			http.Header{"X-Forwarded-For": {" 3.3.3.3 , 4.4.4.4"}},
			"3.3.3.3",
		},
		{
			"no headers",
			http.Header{},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClientIP(tt.headers); got != tt.want {
				t.Errorf("ClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestClientIP_ValidatesAddresses pins that ClientIP returns only well-formed IP
// addresses, in canonical form.
//
// This became load-bearing when cookieless identity made ClientIP's output an
// HMAC input. The hash frames its fields as project ‖ 0x00 ‖ ip ‖ 0x00 ‖ ua,
// which is only injective while no field before the last can contain the
// separator — so an unvalidated ip is what would make it ambiguous. Parsing also
// canonicalises, so two spellings of one address (an IPv6 in short vs expanded
// form, or with a zone) cannot hash to two different visitors.
func TestClientIP_ValidatesAddresses(t *testing.T) {
	for _, c := range []struct {
		name   string
		header string
		value  string
		want   string
	}{
		{"cf_ipv4", HeaderCFConnectingIP, "203.0.113.7", "203.0.113.7"},
		{"cf_ipv6_canonicalised", HeaderCFConnectingIP, "2001:0DB8::0001", "2001:db8::1"},
		{"cf_rejects_nul", HeaderCFConnectingIP, "203.0.113.7\x005.6.7.8", ""},
		{"cf_rejects_garbage", HeaderCFConnectingIP, "not-an-ip", ""},
		{"cf_rejects_empty", HeaderCFConnectingIP, "", ""},
		{"true_client_ip", HeaderTrueClientIP, "198.51.100.4", "198.51.100.4"},
		{"true_client_rejects_garbage", HeaderTrueClientIP, "banana", ""},
		{"xff_first_entry", HeaderXForwardedFor, "203.0.113.7, 70.41.3.18", "203.0.113.7"},
		{"xff_trims_space", HeaderXForwardedFor, "  203.0.113.7  ", "203.0.113.7"},
		{"xff_rejects_garbage", HeaderXForwardedFor, "spoofed, 203.0.113.7", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.value != "" {
				h.Set(c.header, c.value)
			}
			if got := ClientIP(h); got != c.want {
				t.Errorf("ClientIP(%s=%q) = %q, want %q", c.header, c.value, got, c.want)
			}
		})
	}
}

// TestClientIP_FallsThroughInvalidHeader pins that a malformed higher-priority
// header does not mask a valid lower-priority one. Returning "" there would let
// anyone suppress identity resolution by sending one junk header.
func TestClientIP_FallsThroughInvalidHeader(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderCFConnectingIP, "not-an-ip")
	h.Set(HeaderXForwardedFor, "203.0.113.7")
	if got := ClientIP(h); got != "203.0.113.7" {
		t.Errorf("ClientIP = %q, want fall-through to the valid XFF entry %q", got, "203.0.113.7")
	}
}
