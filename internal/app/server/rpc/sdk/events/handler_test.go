package events

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coreevents "github.com/pug-sh/pug/internal/core/events"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/geo"
	"github.com/pug-sh/pug/internal/useragent"
)

// stubJetStream captures the published batch. Only Publish is exercised, so the
// embedded nil interface is never dereferenced.
type stubJetStream struct {
	jetstream.JetStream
	data []byte
}

func (s *stubJetStream) Publish(_ context.Context, _ string, data []byte, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	s.data = data
	return &jetstream.PubAck{}, nil
}

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
		return strconv.FormatBool(x.BoolValue)
	case *commonv1.PropertyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonv1.PropertyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'f', -1, 64)
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
				geo.PropContinent: "NA", geo.PropCountry: "US",
				geo.PropRegion: "California", geo.PropCity: "San Francisco",
				geo.PropPostalCode: "94105", geo.PropMetroCode: "807",
				geo.PropLatitude: "37.7749", geo.PropLongitude: "-122.4194", geo.PropTimezone: "America/Los_Angeles",
			},
			[]*eventsv1.Event{{}},
			map[string]string{
				geo.PropContinent: "NA", geo.PropCountry: "US",
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
			// $ip is personal data and is never stored. Our SDKs never send it, but
			// the SDK endpoint assumes untrusted callers — a hand-crafted request
			// must not be able to smuggle an IP into storage via auto_properties.
			"client-supplied $ip is stripped",
			geo.Location{geo.PropCountry: "US"},
			[]*eventsv1.Event{{AutoProperties: propMap(map[string]string{geo.PropIP: "9.9.9.9", "$browser": "Chrome"})}},
			map[string]string{geo.PropCountry: "US", "$browser": "Chrome"},
		},
		{
			// The strip runs even when the provider returns no location (the
			// empty-location early-return must not skip it).
			"client-supplied $ip stripped with empty location",
			geo.Location{},
			[]*eventsv1.Event{{AutoProperties: propMap(map[string]string{geo.PropIP: "9.9.9.9", "$browser": "Chrome"})}},
			map[string]string{"$browser": "Chrome"},
		},
		{
			// Defense in depth: even if a provider's Location carries $ip, it is
			// dropped before merge and never reaches the event.
			"provider $ip is dropped",
			geo.Location{geo.PropIP: "1.2.3.4", geo.PropCountry: "US"},
			[]*eventsv1.Event{{}},
			map[string]string{geo.PropCountry: "US"},
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
		useragent.PropBrowser:        "Google Chrome",
		useragent.PropBrowserVersion: "118",
		useragent.PropOS:             "Windows",
		useragent.PropOSVersion:      "10",
		useragent.PropMobile:         "false",
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
				useragent.PropBrowser:        "Google Chrome",
				useragent.PropBrowserVersion: "118",
				useragent.PropOS:             "Windows",
				useragent.PropOSVersion:      "10",
				useragent.PropMobile:         "false",
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
				useragent.PropBrowser:        "Google Chrome",
				useragent.PropBrowserVersion: "118",
				useragent.PropOSVersion:      "10",
				useragent.PropMobile:         "false",
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
		useragent.PropBrowser:        "Google Chrome",
		useragent.PropBrowserVersion: "118",
		useragent.PropOSVersion:      "10",
		useragent.PropMobile:         "false",
		// Bot management (CDN headers overwrite client-supplied values).
		"$bot_score":    "5",
		"$verified_bot": "false",
	}

	assertProps(t, events[0], want)
}

func ctxWithProject(ctx context.Context) context.Context {
	return authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypePublicKey,
		Project:  &dbread.Project{ID: "test-project"},
	})
}

// TestBatchCreate_DuplicateEventID verifies that a batch with duplicate event IDs
// is rejected with CodeInvalidArgument and ReasonInvalidEventBatch.
func TestBatchCreate_DuplicateEventID(t *testing.T) {
	s := &Server{
		publisher:   nil, // not reached — validation fails first
		geoProvider: stubProvider{},
		uaParser:    nil,
	}
	req := connect.NewRequest(&eventsv1.BatchCreateRequest{
		Events: []*eventsv1.Event{
			{EventId: proto.String("dup-id")},
			{EventId: proto.String("dup-id")},
		},
	})
	ctx := ctxWithProject(context.Background())
	_, err := s.BatchCreate(ctx, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *apperr.Error, got %T: %v", err, err)
	}
	if ae.Code() != connect.CodeInvalidArgument {
		t.Errorf("want CodeInvalidArgument, got %v", ae.Code())
	}
	if ae.Reason() != apperr.ReasonInvalidEventBatch {
		t.Errorf("want reason %q, got %q", apperr.ReasonInvalidEventBatch, ae.Reason())
	}
}

// TestEnrichBotAndVerified_TypedSlots pins the oneof contract that the regular
// string-comparison tests cannot catch: $bot_score must land in IntValue (not
// StringValue), $verified_bot in BoolValue. A regression that returned the
// String slot for either would round-trip through propString and pass the
// existing tests, then write a String slot to the ClickHouse Variant column.
func TestEnrichBotAndVerified_TypedSlots(t *testing.T) {
	s := &Server{geoProvider: stubProvider{}}

	t.Run("bot_score is IntValue", func(t *testing.T) {
		events := []*eventsv1.Event{{}}
		h := http.Header{}
		h.Set(cfHeaderBotScore, "42")
		s.enrichBotScore(context.Background(), "test-project", h, events)

		got := events[0].AutoProperties["$bot_score"]
		iv, ok := got.GetValue().(*commonv1.PropertyValue_IntValue)
		if !ok {
			t.Fatalf("expected IntValue, got %T", got.GetValue())
		}
		if iv.IntValue != 42 {
			t.Errorf("IntValue = %d, want 42", iv.IntValue)
		}
	})

	t.Run("verified_bot is BoolValue", func(t *testing.T) {
		events := []*eventsv1.Event{{}}
		h := http.Header{}
		h.Set(cfHeaderVerifiedBot, "true")
		s.enrichVerifiedBot(context.Background(), "test-project", h, events)

		got := events[0].AutoProperties["$verified_bot"]
		bv, ok := got.GetValue().(*commonv1.PropertyValue_BoolValue)
		if !ok {
			t.Fatalf("expected BoolValue, got %T", got.GetValue())
		}
		if !bv.BoolValue {
			t.Errorf("BoolValue = false, want true")
		}
	})
}

// TestBatchCreateWiresAttributionEnricher pins that BatchCreate actually RUNS
// enrichAttribution, by asserting on what reaches the publisher. Every other
// attribution test invokes the enricher itself — TestEnrichAttribution calls
// the method directly, and TestEnrichGeoAndUserAgentAndBotScore replays the
// chain by hand — so all of them keep passing if the call is dropped from the
// handler, leaving every ingested event with no $channel/$pathname/
// $referrerDomain forever and no error anywhere. This is the only test that
// fails when the wiring goes.
func TestBatchCreateWiresAttributionEnricher(t *testing.T) {
	js := &stubJetStream{}
	s := &Server{
		publisher:   coreevents.NewPublisher(js),
		geoProvider: stubProvider{},
	}
	req := connect.NewRequest(&eventsv1.BatchCreateRequest{
		Events: []*eventsv1.Event{{
			EventId:    proto.String(uuid.NewString()),
			DistinctId: proto.String("u1"),
			Kind:       proto.String("page_view"),
			OccurTime:  timestamppb.New(time.Unix(1700000000, 0)),
			SessionId:  proto.String(uuid.NewString()),
			AutoProperties: propMap(map[string]string{
				"$url":      "https://shop.example.com/products/ball",
				"$referrer": "https://www.google.com/",
			}),
		}},
	})

	if _, err := s.BatchCreate(ctxWithProject(context.Background()), req); err != nil {
		t.Fatalf("BatchCreate: %v", err)
	}

	var batch eventsv1.EventBatch
	if err := proto.Unmarshal(js.data, &batch); err != nil {
		t.Fatalf("unmarshal published batch: %v", err)
	}
	if len(batch.GetEvents()) != 1 {
		t.Fatalf("published %d events, want 1", len(batch.GetEvents()))
	}
	for key, want := range map[string]string{
		"$pathname":       "/products/ball",
		"$hostname":       "shop.example.com",
		"$referrerDomain": "google.com",
		"$channel":        "Organic Search",
	} {
		if got := propString(batch.GetEvents()[0].AutoProperties[key]); got != want {
			t.Errorf("published event %s = %q, want %q — is enrichAttribution still wired into BatchCreate?", key, got, want)
		}
	}
}

func TestEnrichAttribution(t *testing.T) {
	s := &Server{}

	t.Run("full web pageview derives everything", func(t *testing.T) {
		events := []*eventsv1.Event{{
			AutoProperties: propMap(map[string]string{
				"$url":      "https://Shop.PugAndPals.example.com/products/ball?utm_source=google&utm_medium=cpc&utm_term=dog+food",
				"$referrer": "https://www.google.com/",
				"$locale":   "en_us",
			}),
		}}
		events[0].AutoProperties["$screenWidth"] = &commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: 1920}}
		events[0].AutoProperties["$screenHeight"] = &commonv1.PropertyValue{Value: &commonv1.PropertyValue_IntValue{IntValue: 1080}}

		s.enrichAttribution(events)

		want := map[string]string{
			"$pathname":       "/products/ball",
			"$hostname":       "shop.pugandpals.example.com",
			"$referrerDomain": "google.com",
			"$channel":        "Paid Search",
			"$screenSize":     "1920x1080",
			"$utmSource":      "google",
			"$utmMedium":      "cpc",
			"$utmTerm":        "dog food",
			"$locale":         "en-US",
		}
		for k, v := range want {
			if got := propString(events[0].AutoProperties[k]); got != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
		if _, ok := events[0].AutoProperties["$utmContent"]; ok {
			t.Error("$utmContent must stay absent when neither client nor URL carries it")
		}
	})

	// NormalizeLocale blanks a whitespace-only tag, and the write loop cannot
	// clear a key — so without the pre-delete the raw client value would reach
	// the locale column and become a permanent rollup dimension value.
	t.Run("unnormalizable locale is dropped not stored", func(t *testing.T) {
		events := []*eventsv1.Event{{
			AutoProperties: propMap(map[string]string{"$locale": "   "}),
		}}
		s.enrichAttribution(events)
		if got, ok := events[0].AutoProperties["$locale"]; ok {
			t.Errorf("$locale = %q, want the un-normalizable client value dropped", propString(got))
		}
	})

	t.Run("valid locale still normalized in place", func(t *testing.T) {
		events := []*eventsv1.Event{{
			AutoProperties: propMap(map[string]string{"$locale": "  zh_hans_cn  "}),
		}}
		s.enrichAttribution(events)
		if got := propString(events[0].AutoProperties["$locale"]); got != "zh-Hans-CN" {
			t.Errorf("$locale = %q, want %q", got, "zh-Hans-CN")
		}
	})

	t.Run("server-only keys stripped and rederived", func(t *testing.T) {
		events := []*eventsv1.Event{{
			AutoProperties: propMap(map[string]string{
				"$url":            "https://pugandpals.example.com/",
				"$referrer":       "https://reddit.com/r/pugs",
				"$referrerDomain": "evil.example",
				"$channel":        "Paid Search",
			}),
		}}
		s.enrichAttribution(events)
		if got := propString(events[0].AutoProperties["$referrerDomain"]); got != "reddit.com" {
			t.Errorf("$referrerDomain = %q, want server-derived reddit.com", got)
		}
		if got := propString(events[0].AutoProperties["$channel"]); got != "Organic Social" {
			t.Errorf("$channel = %q, want server-derived Organic Social", got)
		}
	})

	t.Run("server-only keys stripped without url stay absent", func(t *testing.T) {
		events := []*eventsv1.Event{{
			AutoProperties: propMap(map[string]string{
				"$referrerDomain": "evil.example",
				"$channel":        "Email",
			}),
		}}
		s.enrichAttribution(events)
		assertProps(t, events[0], map[string]string{})
	})

	t.Run("client values win for derive-if-absent keys", func(t *testing.T) {
		events := []*eventsv1.Event{{
			AutoProperties: propMap(map[string]string{
				"$url":        "https://pugandpals.example.com/real/path?utm_source=bing",
				"$pathname":   "/logical/route",
				"$hostname":   "app.pugandpals.example.com",
				"$screenSize": "390x844",
				"$utmSource":  "google",
			}),
		}}
		s.enrichAttribution(events)
		for k, v := range map[string]string{
			"$pathname":   "/logical/route",
			"$hostname":   "app.pugandpals.example.com",
			"$screenSize": "390x844",
			"$utmSource":  "google",
		} {
			if got := propString(events[0].AutoProperties[k]); got != v {
				t.Errorf("%s = %q, want client value %q", k, got, v)
			}
		}
	})

	t.Run("self-referral blanks referrer domain and lands direct", func(t *testing.T) {
		events := []*eventsv1.Event{{
			AutoProperties: propMap(map[string]string{
				"$url":      "https://pugandpals.example.com/cart",
				"$referrer": "https://www.pugandpals.example.com/",
			}),
		}}
		s.enrichAttribution(events)
		if _, ok := events[0].AutoProperties["$referrerDomain"]; ok {
			t.Error("$referrerDomain must be absent for a self-referral")
		}
		if got := propString(events[0].AutoProperties["$channel"]); got != "Direct" {
			t.Errorf("$channel = %q, want Direct", got)
		}
	})

	t.Run("non-web event untouched", func(t *testing.T) {
		events := []*eventsv1.Event{{
			AutoProperties: propMap(map[string]string{"$platform": "ios", "$osVersion": "17.4"}),
		}}
		s.enrichAttribution(events)
		assertProps(t, events[0], map[string]string{"$platform": "ios", "$osVersion": "17.4"})
	})

	t.Run("nil auto properties tolerated", func(t *testing.T) {
		events := []*eventsv1.Event{{}}
		s.enrichAttribution(events)
		if len(events[0].AutoProperties) != 0 {
			t.Errorf("expected no auto-properties, got %v", events[0].AutoProperties)
		}
	})
}
