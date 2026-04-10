package events

import (
	"context"
	"net/http"
	"testing"

	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/events/v1"
	"github.com/fivebitsio/cotton/internal/geo"
	"github.com/fivebitsio/cotton/internal/useragent"
)

type stubProvider struct {
	loc geo.Location
}

func (s stubProvider) Locate(http.Header) geo.Location { return s.loc }

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
			[]*eventsv1.Event{{AutoProperties: map[string]string{"$browser": "Chrome"}}},
			map[string]string{geo.PropCountry: "US", "$browser": "Chrome"},
		},
		{
			"geo overwrites existing geo keys",
			geo.Location{geo.PropCountry: "ServerSide"},
			[]*eventsv1.Event{{AutoProperties: map[string]string{geo.PropCountry: "ClientSide"}}},
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
			s.enrichGeo(context.Background(), http.Header{}, tt.events)

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
					} else if gotV != wantV {
						t.Errorf("AutoProperties[%q] = %q, want %q", k, gotV, wantV)
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
		} else if gotV != wantV {
			t.Errorf("AutoProperties[%q] = %q, want %q", k, gotV, wantV)
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
			events:   []*eventsv1.Event{{AutoProperties: map[string]string{"$custom": "value"}}},
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
			events:   []*eventsv1.Event{{AutoProperties: map[string]string{useragent.PropDevice: "Mobile", useragent.PropOS: "iOS"}}},
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
			s.enrichUserAgent(context.Background(), h, tt.events)
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
		wantScore uint32
	}{
		{
			name:      "valid score applied to all events",
			header:    "42",
			events:    []*eventsv1.Event{{}, {}},
			wantScore: 42,
		},
		{
			name:      "score 0 applied",
			header:    "0",
			events:    []*eventsv1.Event{{}},
			wantScore: 0,
		},
		{
			name:      "score 99 applied",
			header:    "99",
			events:    []*eventsv1.Event{{}},
			wantScore: 99,
		},
		{
			name:      "no header — bot_score unchanged",
			header:    "",
			events:    []*eventsv1.Event{{BotScore: 50}},
			wantScore: 50,
		},
		{
			name:      "invalid header — bot_score unchanged",
			header:    "not-a-number",
			events:    []*eventsv1.Event{{BotScore: 10}},
			wantScore: 10,
		},
	}

	s := &Server{geoProvider: stubProvider{}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.header != "" {
				h.Set(cfHeaderBotScore, tt.header)
			}
			s.enrichBotScore(context.Background(), h, tt.events)
			for _, event := range tt.events {
				if event.BotScore != tt.wantScore {
					t.Errorf("BotScore = %d, want %d", event.BotScore, tt.wantScore)
				}
			}
		})
	}
}

func TestEnrichGeoAndUserAgent(t *testing.T) {
	uaParser, err := useragent.NewParser()
	if err != nil {
		t.Fatal(err)
	}

	loc := geo.Location{geo.PropCountry: "US", geo.PropCity: "San Francisco"}
	s := &Server{geoProvider: stubProvider{loc: loc}, uaParser: uaParser}

	events := []*eventsv1.Event{{AutoProperties: map[string]string{useragent.PropOS: "iOS"}}}

	h := http.Header{}
	h.Set("User-Agent", chromeWindowsUA)

	// Same order as BatchCreate: geo first, then UA.
	s.enrichGeo(context.Background(), h, events)
	s.enrichUserAgent(context.Background(), h, events)

	want := map[string]string{
		// Geo props (always overwrite).
		geo.PropCountry: "US",
		geo.PropCity:    "San Francisco",
		// UA props (skip existing keys — client-supplied OS preserved).
		useragent.PropOS:             "iOS",
		useragent.PropBrowser:        "Chrome",
		useragent.PropBrowserVersion: "118",
		useragent.PropOSVersion:      "10",
	}

	assertProps(t, events[0], want)
}
