package botdetect

import (
	"strings"
	"testing"

	agents "github.com/monperrus/crawler-user-agents"
)

const (
	chromeUA   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	headlessUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/120.0.0.0 Safari/537.36"
)

func TestMatchUserAgent(t *testing.T) {
	cases := []struct {
		name, ua, want string
	}{
		{"chrome", chromeUA, ""},
		{"empty", "", ""},
		{"headless chrome", headlessUA, "HeadlessChrome"},
		{"lighthouse", chromeUA + " Chrome-Lighthouse", "Chrome-Lighthouse"},
		{"datadog synthetics", chromeUA + " DatadogSynthetics", "DatadogSynthetics"},
		// Regex-source patterns (Googlebot\/, ^curl) report the matched name.
		{"googlebot", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "Googlebot"},
		{"curl", "curl/8.4.0", "curl"},
		{"oversized", strings.Repeat("x", maxUALen) + headlessUA, ""},
		// HTTP-client libraries are on the list too — what native and server
		// SDKs send — which is why the enricher only tags $platform=web events.
		{"okhttp (React Native Android)", "okhttp/4.12.0", "okhttp"},
		{"go http client", "Go-http-client/2.0", "Go-http-client"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := MatchUserAgent(c.ua)
			if ok != (c.want != "") || got != c.want {
				t.Errorf("MatchUserAgent(%q) = %q, %v; want %q", c.ua, got, ok, c.want)
			}
		})
	}
}

// TestMatchUserAgentReasonsAreNames runs every example user agent the list
// ships: a reason is never empty and never the pattern's regex source.
func TestMatchUserAgentReasonsAreNames(t *testing.T) {
	for _, c := range agents.Crawlers {
		for _, ua := range c.Instances {
			reason, ok := MatchUserAgent(ua)
			if !ok || reason == "" || strings.Contains(reason, `\`) {
				t.Errorf("MatchUserAgent(%q) = %q, %v (pattern %q)", ua, reason, ok, c.Pattern)
			}
		}
	}
}

func TestMatchASN(t *testing.T) {
	cases := []struct {
		asn  uint64
		want string
	}{
		{24940, "asn:24940"},
		{16509, "asn:16509"},
		{15169, ""}, // Google: Chrome IP Protection, Google Fi
		{13335, ""}, // Cloudflare: WARP, iCloud Private Relay
		{7922, ""},  // Comcast
		{0, ""},
	}
	for _, c := range cases {
		got, ok := MatchASN(c.asn)
		if ok != (c.want != "") || got != c.want {
			t.Errorf("MatchASN(%d) = %q, %v; want %q", c.asn, got, ok, c.want)
		}
	}
}
