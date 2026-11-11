package useragent

import (
	"net/http"
	"strings"
	"testing"
)

func header(ua string) http.Header {
	h := http.Header{}
	if ua != "" {
		h.Set("User-Agent", ua)
	}
	return h
}

func TestNewParser(t *testing.T) {
	p, err := NewParser()
	if err != nil {
		t.Fatalf("NewParser() returned error: %v", err)
	}
	if p == nil {
		t.Fatal("NewParser() returned nil parser")
	}
}

func TestParse(t *testing.T) {
	p, err := NewParser()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		ua   string
		want Properties
	}{
		{
			name: "chrome desktop: browser and os, no device, NT 10.0 names no version",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36",
			want: Properties{
				PropBrowser:        "Google Chrome",
				PropBrowserVersion: "118",
				PropOS:             "Windows",
				PropMobile:         "false",
			},
		},
		{
			name: "iphone safari — browser normalized to Safari, mobile true",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
			want: Properties{
				PropBrowser:        "Safari",
				PropBrowserVersion: "17",
				PropOS:             "iOS",
				PropOSVersion:      "17.0",
				PropDevice:         "iPhone",
				PropMobile:         "true",
			},
		},
		{
			name: "ipad safari — tablet is not mobile, browser Safari",
			ua:   "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
			want: Properties{
				PropBrowser:        "Safari",
				PropBrowserVersion: "17",
				PropOS:             "iOS",
				PropOSVersion:      "17.0",
				PropDevice:         "iPad",
				PropMobile:         "false",
			},
		},
		{
			name: "android chrome — browser, os, and device",
			ua:   "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
			want: Properties{
				PropBrowser:        "Google Chrome",
				PropBrowserVersion: "120",
				PropOS:             "Android",
				PropOSVersion:      "14",
				PropDevice:         "Pixel 8",
				PropMobile:         "true",
			},
		},
		{
			name: "firefox on ubuntu — distro folded to Linux",
			ua:   "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:126.0) Gecko/20100101 Firefox/126.0",
			want: Properties{
				PropBrowser:        "Firefox",
				PropBrowserVersion: "126",
				PropOS:             "Linux",
				PropMobile:         "false",
			},
		},
		{
			name: "chrome os: not folded to Linux, reduced 14541.0.0 names no version",
			ua:   "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
			want: Properties{
				PropBrowser:        "Google Chrome",
				PropBrowserVersion: "124",
				PropOS:             "Chrome OS",
				PropMobile:         "false",
			},
		},
		{
			name: "firefox on macos: capped 10.15 names no version",
			ua:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:158.0) Gecko/20100101 Firefox/158.0",
			want: Properties{
				PropBrowser:        "Firefox",
				PropBrowserVersion: "158",
				PropOS:             "macOS",
				PropDevice:         "Mac",
				PropMobile:         "false",
			},
		},
		{
			name: "safari on macos: frozen 10_15_7 names no version",
			ua:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15",
			want: Properties{
				PropBrowser:        "Safari",
				PropBrowserVersion: "26",
				PropOS:             "macOS",
				PropDevice:         "Mac",
				PropMobile:         "false",
			},
		},
		{
			name: "safari on mojave: below the cap, version kept",
			ua:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_14_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.1.2 Safari/605.1.15",
			want: Properties{
				PropBrowser:        "Safari",
				PropBrowserVersion: "14",
				PropOS:             "macOS",
				PropOSVersion:      "10.14.6",
				PropDevice:         "Mac",
				PropMobile:         "false",
			},
		},
		{
			name: "firefox on windows 7: pre-NT-10 version kept",
			ua:   "Mozilla/5.0 (Windows NT 6.1; Win64; x64; rv:115.0) Gecko/20100101 Firefox/115.0",
			want: Properties{
				PropBrowser:        "Firefox",
				PropBrowserVersion: "115",
				PropOS:             "Windows",
				PropOSVersion:      "7",
				PropMobile:         "false",
			},
		},
		{
			name: "android chrome reduced UA: frozen release and K device dropped",
			ua:   "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36",
			want: Properties{
				PropBrowser:        "Google Chrome",
				PropBrowserVersion: "141",
				PropOS:             "Android",
				PropMobile:         "true",
			},
		},
		{
			name: "android webview reduced UA: frozen release and placeholder device dropped",
			ua:   "Mozilla/5.0 (Linux; Android 10; K; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/141.0.0.0 Mobile Safari/537.36",
			want: Properties{
				PropBrowser:        "Chrome Mobile WebView",
				PropBrowserVersion: "141",
				PropOS:             "Android",
				PropMobile:         "true",
			},
		},
		{
			name: "firefox on android 10: real release and device kept, only Chromium's K is reduced",
			ua:   "Mozilla/5.0 (Android 10; Mobile; rv:144.0) Gecko/144.0 Firefox/144.0",
			want: Properties{
				PropBrowser:        "Firefox Mobile",
				PropBrowserVersion: "144",
				PropOS:             "Android",
				PropOSVersion:      "10",
				PropDevice:         "Generic Smartphone",
				PropMobile:         "true",
			},
		},
		{
			name: "ios safari 26: frozen 18.x replaced by Safari's version",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 18_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.2 Mobile/15E148 Safari/604.1",
			want: Properties{
				PropBrowser:        "Safari",
				PropBrowserVersion: "26",
				PropOS:             "iOS",
				PropOSVersion:      "26.2",
				PropDevice:         "iPhone",
				PropMobile:         "true",
			},
		},
		{
			name: "ios safari 18: os token kept below Safari 26, patch included",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 18_6_2 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Mobile/15E148 Safari/604.1",
			want: Properties{
				PropBrowser:        "Safari",
				PropBrowserVersion: "18",
				PropOS:             "iOS",
				PropOSVersion:      "18.6.2",
				PropDevice:         "iPhone",
				PropMobile:         "true",
			},
		},
		{
			name: "chrome on ios: no Safari version token, os token kept",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/126.0.6478.54 Mobile/15E148 Safari/604.1",
			want: Properties{
				PropBrowser:        "Chrome Mobile iOS",
				PropBrowserVersion: "126",
				PropOS:             "iOS",
				PropOSVersion:      "17.5",
				PropDevice:         "iPhone",
				PropMobile:         "true",
			},
		},
		{
			name: "ios app webview: an app's AppVersion token is not Safari's version",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 MyApp/5.2 AppVersion/2025.40",
			want: Properties{
				PropBrowser:   "Mobile Safari UI/WKWebView",
				PropOS:        "iOS",
				PropOSVersion: "17.5",
				PropDevice:    "iPhone",
				PropMobile:    "true",
			},
		},
		{
			name: "ios app webview: a Version/ token without Safari's is not Safari's version",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 MyApp/5.2 Version/26.3",
			want: Properties{
				PropBrowser:   "Mobile Safari UI/WKWebView",
				PropOS:        "iOS",
				PropOSVersion: "17.5",
				PropDevice:    "iPhone",
				PropMobile:    "true",
			},
		},
		{
			name: "ios 26 app webview: frozen 18.7 dropped, no Safari version to read",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 18_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148",
			want: Properties{
				PropBrowser: "Mobile Safari UI/WKWebView",
				PropOS:      "iOS",
				PropDevice:  "iPhone",
				PropMobile:  "true",
			},
		},
		{
			name: "googlebot — browser and device (Spider)",
			ua:   "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
			want: Properties{
				PropBrowser:        "Googlebot",
				PropBrowserVersion: "2",
				PropDevice:         "Spider",
				PropMobile:         "false",
			},
		},
		{
			name: "empty header — returns nil",
			ua:   "",
			want: nil,
		},
		{
			name: "garbage string — returns nil (all Other)",
			ua:   "not-a-real-user-agent",
			want: nil,
		},
		{
			name: "curl — browser only, no os or device",
			ua:   "curl/8.1.2",
			want: Properties{
				PropBrowser:        "curl",
				PropBrowserVersion: "8",
				PropMobile:         "false",
			},
		},
		{
			name: "token ending at the length cap: parsed",
			ua:   strings.Repeat("x", maxUALen-len(" Firefox/120.0")) + " Firefox/120.0",
			want: Properties{
				PropBrowser:        "Firefox",
				PropBrowserVersion: "120",
				PropMobile:         "false",
			},
		},
		{
			name: "token past the length cap: ignored",
			ua:   strings.Repeat("x", maxUALen) + " Firefox/120.0",
			want: nil,
		},
		{
			name: "invalid UTF-8 byte: stripped",
			ua:   "Mozilla/5.0 (Linux; Android 14; Pixel\xff 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
			want: Properties{
				PropBrowser:        "Google Chrome",
				PropBrowserVersion: "120",
				PropOS:             "Android",
				PropOSVersion:      "14",
				PropDevice:         "Pixel 8",
				PropMobile:         "true",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Parse(header(tt.ua))
			if tt.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %v, got nil", tt.want)
			}
			if len(got) != len(tt.want) {
				t.Errorf("got %d keys, want %d\ngot:  %v\nwant: %v", len(got), len(tt.want), got, tt.want)
				return
			}
			for k, wantV := range tt.want {
				if gotV, ok := got[k]; !ok {
					t.Errorf("missing key %q", k)
				} else if gotV != wantV {
					t.Errorf("%q = %q, want %q", k, gotV, wantV)
				}
			}
		})
	}
}

func TestParseNilReceiver(t *testing.T) {
	var p *Parser
	got := p.Parse(header("Mozilla/5.0"))
	if got != nil {
		t.Errorf("nil parser should return nil, got %v", got)
	}
}

func TestFromBrowserEngine(t *testing.T) {
	tests := []struct {
		ua   string
		want bool
	}{
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36", true},
		{"Mozilla/5.0 (Linux; Android 10; K; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/141.0.0.0 Mobile Safari/537.36", true},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148", true},
		{"okhttp/4.12.0", false},
		{"Runner/1 CFNetwork/1568.100.1 Darwin/24.0.0", false},
		{"Dart/3.5 (dart:io)", false},
		{"pug-android/1.0.0", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := FromBrowserEngine(header(tt.ua)); got != tt.want {
			t.Errorf("FromBrowserEngine(%q) = %v, want %v", tt.ua, got, tt.want)
		}
	}
}
