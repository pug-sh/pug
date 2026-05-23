package useragent

import (
	"net/http"
	"testing"
)

func TestParseClientHints(t *testing.T) {
	tests := []struct {
		name string
		h    http.Header
		want Properties
	}{
		{
			name: "chrome desktop — matches web SDK UA-CH shape",
			h: http.Header{
				"Sec-Ch-Ua":                 {`"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`},
				"Sec-Ch-Ua-Mobile":            {"?0"},
				"Sec-Ch-Ua-Platform":        {`"Windows"`},
				"Sec-Ch-Ua-Platform-Version": {`"15.0.0"`},
				"Sec-Ch-Ua-Model":           {`""`},
			},
			want: Properties{
				PropBrowser:        "Google Chrome",
				PropBrowserVersion: "131",
				PropOS:             "Windows",
				PropOSVersion:      "15.0.0",
				PropMobile:         "false",
			},
		},
		{
			name: "android phone — model and mobile set",
			h: http.Header{
				"Sec-Ch-Ua":          {`"Google Chrome";v="120", "Chromium";v="120", "Not_A Brand";v="24"`},
				"Sec-Ch-Ua-Mobile":   {"?1"},
				"Sec-Ch-Ua-Platform": {`"Android"`},
				"Sec-Ch-Ua-Model":    {`"Pixel 8"`},
			},
			want: Properties{
				PropBrowser:        "Google Chrome",
				PropBrowserVersion: "120",
				PropOS:             "Android",
				PropDevice:         "Pixel 8",
				PropMobile:         "true",
			},
		},
		{
			name: "missing Sec-Ch-Ua — returns nil",
			h: http.Header{
				"Sec-Ch-Ua-Platform": {`"Windows"`},
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseClientHints(tt.h)
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

func TestParsePrefersClientHints(t *testing.T) {
	p, err := NewParser()
	if err != nil {
		t.Fatal(err)
	}

	h := http.Header{
		"User-Agent":                {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36"},
		"Sec-Ch-Ua":                 {`"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`},
		"Sec-Ch-Ua-Mobile":          {"?0"},
		"Sec-Ch-Ua-Platform":        {`"Windows"`},
		"Sec-Ch-Ua-Platform-Version": {`"15.0.0"`},
	}

	got := p.Parse(h)
	want := Properties{
		PropBrowser:        "Google Chrome",
		PropBrowserVersion: "131",
		PropOS:             "Windows",
		PropOSVersion:      "15.0.0",
		PropMobile:         "false",
	}

	for k, wantV := range want {
		if gotV, ok := got[k]; !ok {
			t.Errorf("missing key %q", k)
		} else if gotV != wantV {
			t.Errorf("%q = %q, want %q", k, gotV, wantV)
		}
	}
	if _, ok := got[PropDevice]; ok {
		t.Errorf("desktop client hints should not set %q", PropDevice)
	}
}
