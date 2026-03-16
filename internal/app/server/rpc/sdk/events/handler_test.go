package events

import (
	"context"
	"net/http"
	"testing"

	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/events/v1"
	"github.com/fivebitsio/cotton/internal/geo"
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
