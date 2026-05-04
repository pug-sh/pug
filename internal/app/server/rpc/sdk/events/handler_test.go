package events

import (
	"context"
	"net/http"
	"testing"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
	"github.com/pug-sh/pug/internal/geo"
	"github.com/pug-sh/pug/internal/useragent"
)

type stubProvider struct {
	loc geo.Location
}

func (s stubProvider) Locate(http.Header) geo.Location { return s.loc }

func propValue(v string) *commonv1.PropertyValue {
	return &commonv1.PropertyValue{
		Value: &commonv1.PropertyValue_StringValue{StringValue: v},
	}
}

func propMap(m map[string]string) map[string]*commonv1.PropertyValue {
	if m == nil {
		return nil
	}
	out := make(map[string]*commonv1.PropertyValue, len(m))
	for k, v := range m {
		out[k] = propValue(v)
	}
	return out
}

func propString(v *commonv1.PropertyValue) string {
	if v == nil {
		return ""
	}
	switch x := v.GetValue().(type) {
	case *commonv1.PropertyValue_StringValue:
		return x.StringValue
	case *commonv1.PropertyValue_BoolValue:
		if x.BoolValue {
			return "true"
		}
		return "false"
	case *commonv1.PropertyValue_IntValue:
		return ""
	case *commonv1.PropertyValue_DoubleValue:
		return ""
	default:
		return ""
	}
}

func TestEnrichGeo(t *testing.T) {
	tests := []struct {
		name   string
		loc    geo.Location
		events []*eventsv1.Event
		want   map[string]string // expected auto-properties on each event (nil = no geo keys)
	}{
		{
			"all geo fields set",
			geo.Location{
				geo.PropIP: "1.2.3.4", geo.PropContinent: "NA", geo.PropCountry: "US",
				geo.PropRegion: "California", geo.PropCity: "San Francisco",
				geo.PropPostalCode: "94105", geo.PropMetroCode: "807",
				geo.PropLatitude: "37.7749", geo.PropLongitude: "-122.4194", geo.PropTimezone: "America/Los_Angeles",
			},
			[]*eventsv1.Event{{}},
			map[string]string{
				geo.PropIP: "1.2.3.4", geo.PropContinent: "NA", geo.PropCountry: "US",
				geo.PropRegion: "California", geo.PropCity: "San Francisco",
				geo.PropPostalCode: "94105", geo.PropMetroCode: "807",
				geo.PropLatitude: "37.7749", geo.PropLongitude: "-122.4194", geo.PropTimezone: "America/Los_Angeles",
			},
		},
		{
			"partial geo — country only",
			geo.Location{geo.PropCountry: "DE"},
			[]*eventsv1.Event{{}},
			map[string]string{geo.PropCountry: "DE"},
		},
		{
			"empty location — no properties set",
			geo.Location{},
			[]*eventsv1.Event{{}},
			nil,
		},
		{
			"nil location — no properties set",
			nil,
			[]*eventsv1.Event{{}},
			nil,
		},
		{
			"multiple events enriched",
			geo.Location{geo.PropCountry: "JP"},
			[]*eventsv1.Event{{}, {}},
			map[string]string{geo.PropCountry: "JP"},
		},
		{
			"preserves existing auto-properties",
			geo.Location{geo.PropCountry: "US"},
			[]*eventsv1.Event{{AutoProperties: propMap(map[string]string{"$browser": "Chrome"})}},
			map[string]string{geo.PropCountry: "US", "$browser": "Chrome"},
		},
		{
			"geo overwrites existing geo keys",
			geo.Location{geo.PropCountry: "ServerSide"},
			[]*eventsv1.Event{{AutoProperties: propMap(map[string]string{geo.PropCountry: "ClientSide"})}},
			map[string]string{geo.PropCountry: "ServerSide"},
		},
		{
			"metro targeting fields",
			geo.Location{geo.PropCountry: "US", geo.PropPostalCode: "10001", geo.PropMetroCode: "501"},
			[]*eventsv1.Event{{}},
			map[string]string{geo.PropCountry: "US", geo.PropPostalCode: "10001", geo.PropMetroCode: "501"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{geoProvider: stubProvider{loc: tt.loc}}
			s.enrichGeo(context.Background(), "test-project", http.Header{}, tt.events)

			for _, event := range tt.events {
				if tt.want == nil {
					if len(event.AutoProperties) != 0 {
						t.Errorf("expected no auto-properties, got %v", event.AutoProperties)
					}
					continue
				}
				if len(event.AutoProperties) != len(tt.want) {
					t.Errorf("AutoProperties has %d keys, want %d\ngot:  %v\nwant: %v",
						len(event.AutoProperties), len(tt.want), event.AutoProperties, tt.want)
					continue
				}
				for k, wantV := range tt.want {
					gotV, ok := event.AutoProperties[k]
					if !ok {
						t.Errorf("missing expected key %q in AutoProperties", k)
					} else if got := propString(gotV); got != wantV {
						t.Errorf("AutoProperties[%q] = %q, want %q", k, got, wantV)
					}
				}
			}
		})
	}
}

const chromeWindowsUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36"

func assertProps(t *testing.T, event *eventsv1.Event, want map[string]string) {
	t.Helper()
	if want == nil {
		if len(event.AutoProperties) != 0 {
			t.Errorf("expected no auto-properties, got %v", event.AutoProperties)
		}
		return
	}
	if len(event.AutoProperties) != len(want) {
		t.Errorf("AutoProperties has %d keys, want %d\ngot:  %v\nwant: %v",
			len(event.AutoProperties), len(want), event.AutoProperties, want)
		return
	}
	for k, wantV := range want {
		gotV, ok := event.AutoProperties[k]
		if !ok {
			t.Errorf("missing expected key %q in AutoProperties", k)
		} else if got := propString(gotV); got != wantV {
			t.Errorf("AutoProperties[%q] = %q, want %q", k, got, wantV)
		}
	}
}

func TestEnrichUserAgent(t *testing.T) {
	chromeProps := map[string]string{
		useragent.PropBrowser:        "Chrome",
		useragent.PropBrowserVersion: "118",
		useragent.PropOS:             "Windows",
		useragent.PropOSVersion:      "10",
	}

	tests := []struct {
		name     string
		uaHeader string // empty = omit header
		events   []*eventsv1.Event
		want     map[string]string // expected auto-properties on each event (nil = no UA props)
	}{
		{
			name:     "chrome desktop — browser, os, osVersion set (no device for desktop)",
			uaHeader: chromeWindowsUA,
			events:   []*eventsv1.Event{{}},
			want:     chromeProps,
		},
		{
			name:     "no user-agent header — no properties set",
			uaHeader: "",
			events:   []*eventsv1.Event{{}},
			want:     nil,
		},
		{
			name:     "multiple events all enriched",
			uaHeader: chromeWindowsUA,
			events:   []*eventsv1.Event{{}, {}},
			want:     chromeProps,
		},
		{
			name:     "preserves other auto-properties",
			uaHeader: chromeWindowsUA,
			events:   []*eventsv1.Event{{AutoProperties: propMap(map[string]string{"$custom": "value"})}},
			want: map[string]string{
				"$custom":                    "value",
				useragent.PropBrowser:        "Chrome",
				useragent.PropBrowserVersion: "118",
				useragent.PropOS:             "Windows",
				useragent.PropOSVersion:      "10",
			},
		},
		{
			// Mobile SDKs send device props explicitly; server must not overwrite them.
			name:     "client-supplied ua props not overwritten",
			uaHeader: chromeWindowsUA,
			events:   []*eventsv1.Event{{AutoProperties: propMap(map[string]string{useragent.PropDevice: "Mobile", useragent.PropOS: "iOS"})}},
			want: map[string]string{
				useragent.PropDevice:         "Mobile", // client value preserved
				useragent.PropOS:             "iOS",    // client value preserved
				useragent.PropBrowser:        "Chrome",
				useragent.PropBrowserVersion: "118",
				useragent.PropOSVersion:      "10",
			},
		},
	}

	uaParser, err := useragent.NewParser()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{geoProvider: stubProvider{}, uaParser: uaParser}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.uaHeader != "" {
				h.Set("User-Agent", tt.uaHeader)
			}
			s.enrichUserAgent(context.Background(), "test-project", h, tt.events)
			for _, event := range tt.events {
				assertProps(t, event, tt.want)
			}
		})
	}
}

func TestEnrichBotScore(t *testing.T) {
	tests := []struct {
		name      string
		header    string // empty = omit header
		events    []*eventsv1.Event
		wantScore string // empty = expect absent
	}{
		{
			name:      "valid score applied to all events",
			header:    "42",
			events:    []*eventsv1.Event{{}, {}},
			wantScore: "42",
		},
		{
			name:      "score 0 applied (existing value overwritten)",
			header:    "0",
			events:    []*eventsv1.Event{{AutoProperties: propMap(map[string]string{"$bot_score": "99"})}},
			wantScore: "0",
		},
		{
			name:      "score 99 applied",
			header:    "99",
			events:    []*eventsv1.Event{{}},
			wantScore: "99",
		},
		{
			name:      "no header — client-supplied bot_score stripped",
			header:    "",
			events:    []*eventsv1.Event{{AutoProperties: propMap(map[string]string{"$bot_score": "50"})}},
			wantScore: "",
		},
		{
			name:      "invalid header — bot_score absent",
			header:    "not-a-number",
			events:    []*eventsv1.Event{{AutoProperties: propMap(map[string]string{"$bot_score": "10"})}},
			wantScore: "",
		},
		{
			name:      "out of range for UInt8 — bot_score absent",
			header:    "256",
			events:    []*eventsv1.Event{{AutoProperties: propMap(map[string]string{"$bot_score": "10"})}},
			wantScore: "",
		},
		{
			name:      "negative value — bot_score absent",
			header:    "-1",
			events:    []*eventsv1.Event{{}},
			wantScore: "",
		},
		{
			name:      "max valid score 255",
			header:    "255",
			events:    []*eventsv1.Event{{}},
			wantScore: "255",
		},
		{
			name:      "score 0 on fresh event",
			header:    "0",
			events:    []*eventsv1.Event{{}},
			wantScore: "0",
		},
		{
			name:      "whitespace in header rejected",
			header:    " 42 ",
			events:    []*eventsv1.Event{{}},
			wantScore: "",
		},
		{
			name:      "preserves other auto-properties",
			header:    "42",
			events:    []*eventsv1.Event{{AutoProperties: propMap(map[string]string{"$browser": "Chrome"})}},
			wantScore: "42",
		},
		{
			name:      "no header, nil AutoProperties — no panic",
			header:    "",
			events:    []*eventsv1.Event{{}},
			wantScore: "",
		},
	}

	s := &Server{geoProvider: stubProvider{}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.header != "" {
				h.Set(cfHeaderBotScore, tt.header)
			}
			s.enrichBotScore(context.Background(), "test-project", h, tt.events)
			for _, event := range tt.events {
				got, exists := event.AutoProperties["$bot_score"]
				if tt.wantScore == "" {
					if exists {
						t.Errorf("expected $bot_score absent, got %q", got)
					}
				} else if !exists {
					t.Errorf("expected $bot_score = %q, got absent", tt.wantScore)
				} else if got := propString(got); got != tt.wantScore {
					t.Errorf("$bot_score = %q, want %q", got, tt.wantScore)
				}
			}
		})
	}
}

func TestEnrichVerifiedBot(t *testing.T) {
	tests := []struct {
		name   string
		header string // empty = omit header
		events []*eventsv1.Event
		want   string // empty = expect absent
	}{
		{
			name:   "true applied to all events",
			header: "true",
			events: []*eventsv1.Event{{}, {}},
			want:   "true",
		},
		{
			name:   "false applied",
			header: "false",
			events: []*eventsv1.Event{{}},
			want:   "false",
		},
		{
			name:   "no header — client-supplied value stripped",
			header: "",
			events: []*eventsv1.Event{{AutoProperties: propMap(map[string]string{"$verified_bot": "true"})}},
			want:   "",
		},
		{
			name:   "invalid header — value absent",
			header: "yes",
			events: []*eventsv1.Event{{}},
			want:   "",
		},
		{
			name:   "client-supplied value overwritten",
			header: "false",
			events: []*eventsv1.Event{{AutoProperties: propMap(map[string]string{"$verified_bot": "true"})}},
			want:   "false",
		},
		{
			name:   "preserves other auto-properties",
			header: "true",
			events: []*eventsv1.Event{{AutoProperties: propMap(map[string]string{"$browser": "Chrome"})}},
			want:   "true",
		},
		{
			name:   "no header, nil AutoProperties — no panic",
			header: "",
			events: []*eventsv1.Event{{}},
			want:   "",
		},
		{
			name:   "case-sensitive — True rejected",
			header: "True",
			events: []*eventsv1.Event{{}},
			want:   "",
		},
	}

	s := &Server{geoProvider: stubProvider{}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.header != "" {
				h.Set(cfHeaderVerifiedBot, tt.header)
			}
			s.enrichVerifiedBot(context.Background(), "test-project", h, tt.events)
			for _, event := range tt.events {
				got, exists := event.AutoProperties["$verified_bot"]
				if tt.want == "" {
					if exists {
						t.Errorf("expected $verified_bot absent, got %q", got)
					}
				} else if !exists {
					t.Errorf("expected $verified_bot = %q, got absent", tt.want)
				} else if got := propString(got); got != tt.want {
					t.Errorf("$verified_bot = %q, want %q", got, tt.want)
				}
			}
		})
	}
}

func TestEnrichGeoAndUserAgentAndBotScore(t *testing.T) {
	uaParser, err := useragent.NewParser()
	if err != nil {
		t.Fatal(err)
	}

	loc := geo.Location{geo.PropCountry: "US", geo.PropCity: "San Francisco"}
	s := &Server{geoProvider: stubProvider{loc: loc}, uaParser: uaParser}

	events := []*eventsv1.Event{{
		AutoProperties: propMap(map[string]string{useragent.PropOS: "iOS", "$bot_score": "99", "$verified_bot": "true"}),
	}}

	h := http.Header{}
	h.Set("User-Agent", chromeWindowsUA)
	h.Set(cfHeaderBotScore, "5")
	h.Set(cfHeaderVerifiedBot, "false")

	// Same order as BatchCreate: geo, UA, bot score, verified bot.
	s.enrichGeo(context.Background(), "test-project", h, events)
	s.enrichUserAgent(context.Background(), "test-project", h, events)
	s.enrichBotScore(context.Background(), "test-project", h, events)
	s.enrichVerifiedBot(context.Background(), "test-project", h, events)

	want := map[string]string{
		// Geo props (always overwrite).
		geo.PropCountry: "US",
		geo.PropCity:    "San Francisco",
		// UA props (skip existing keys — client-supplied OS preserved).
		useragent.PropOS:             "iOS",
		useragent.PropBrowser:        "Chrome",
		useragent.PropBrowserVersion: "118",
		useragent.PropOSVersion:      "10",
		// Bot management (CDN headers overwrite client-supplied values).
		"$bot_score":    "5",
		"$verified_bot": "false",
	}

	assertProps(t, events[0], want)
}
