package useragent

import (
	"net/http"
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
			name: "chrome desktop — browser and os, no device",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36",
			want: Properties{
				PropBrowser:        "Chrome",
				PropBrowserVersion: "118",
				PropOS:             "Windows",
				PropOSVersion:      "10",
			},
		},
		{
			name: "iphone safari — browser, os, and device populated",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
			want: Properties{
				PropBrowser:        "Mobile Safari",
				PropBrowserVersion: "17",
				PropOS:             "iOS",
				PropOSVersion:      "17",
				PropDevice:         "iPhone",
			},
		},
		{
			name: "android chrome — browser, os, and device",
			ua:   "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
			want: Properties{
				PropBrowser:        "Chrome Mobile",
				PropBrowserVersion: "120",
				PropOS:             "Android",
				PropOSVersion:      "14",
				PropDevice:         "Pixel 8",
			},
		},
		{
			name: "googlebot — browser and device (Spider)",
			ua:   "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
			want: Properties{
				PropBrowser:        "Googlebot",
				PropBrowserVersion: "2",
				PropDevice:         "Spider",
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
