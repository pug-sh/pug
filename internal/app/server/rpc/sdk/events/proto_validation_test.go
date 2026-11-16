package events

import (
	"slices"
	"strings"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/crawler"
	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
)

func validEvent() *eventsv1.Event {
	return &eventsv1.Event{
		EventId:    proto.String("f47ac10b-58cc-4372-a567-0e02b2c3d479"),
		DistinctId: proto.String("anon-u1"),
		Kind:       proto.String("page_view"),
		OccurTime:  timestamppb.New(time.Unix(1700000000, 0)),
		SessionId:  proto.String("f47ac10b-58cc-4372-a567-0e02b2c3d480"),
	}
}

// crawlerRequest is a crawler request as @pug-sh/crawlers sends it.
func crawlerRequest() *eventsv1.Event {
	return &eventsv1.Event{
		EventId:        proto.String("f47ac10b-58cc-4372-a567-0e02b2c3d484"),
		Kind:           proto.String(crawler.EventKind),
		OccurTime:      timestamppb.New(time.Unix(1700000000, 0)),
		AutoProperties: propMap(map[string]string{"$url": "https://example.com/pricing"}),
		Client:         &eventsv1.Client{UserAgent: proto.String("GPTBot/1.2"), Ip: proto.String("20.171.207.5")},
	}
}

func cookielessEvent() *eventsv1.Event {
	return &eventsv1.Event{
		EventId:    proto.String("f47ac10b-58cc-4372-a567-0e02b2c3d481"),
		Kind:       proto.String("page_view"),
		OccurTime:  timestamppb.New(time.Unix(1700000000, 0)),
		Cookieless: proto.Bool(true),
	}
}

// resolvedCookielessEvent is the post-ingest shape: cookieless flag retained,
// identity filled by the server with the reserved prefix.
func resolvedCookielessEvent() *eventsv1.Event {
	e := cookielessEvent()
	e.DistinctId = proto.String("cookieless-3q2-8pTkXhWZbA4NUJ9wA")
	e.SessionId = proto.String("f47ac10b-58cc-4372-a567-0e02b2c3d482")
	return e
}

func incoming(e *eventsv1.Event) *eventsv1.IncomingEvent {
	return &eventsv1.IncomingEvent{Event: e}
}

func TestEventValidation_TwoStageContract(t *testing.T) {
	cases := []struct {
		name    string
		msg     proto.Message
		wantErr string // substring of the violation id/message; "" = must pass
	}{
		{"sdk_normal_event_ok",
			incoming(validEvent()), ""},
		{"sdk_cookieless_empty_identity_ok",
			incoming(cookielessEvent()), ""},
		{"sdk_normal_missing_distinct_id_rejected",
			incoming(func() *eventsv1.Event {
				e := validEvent()
				e.DistinctId = nil
				return e
			}()), "required unless cookieless"},
		{"sdk_cookieless_with_distinct_id_rejected",
			incoming(func() *eventsv1.Event {
				e := cookielessEvent()
				e.DistinctId = proto.String("anon-u1")
				return e
			}()), "must not send distinct_id"},
		{"sdk_cookieless_with_session_id_rejected",
			incoming(func() *eventsv1.Event {
				e := cookielessEvent()
				e.SessionId = proto.String("f47ac10b-58cc-4372-a567-0e02b2c3d483")
				return e
			}()), "must not send distinct_id"},
		{"sdk_spoofed_prefix_rejected",
			incoming(func() *eventsv1.Event {
				e := validEvent()
				e.DistinctId = proto.String("cookieless-fake")
				return e
			}()), "reserved 'cookieless-' prefix"},
		{"sdk_crawler_prefix_rejected",
			incoming(func() *eventsv1.Event {
				e := validEvent()
				e.DistinctId = proto.String(crawler.DistinctIDPrefix + "gptbot")
				return e
			}()), "reserved 'crawler-' prefix"},
		{"sdk_crawler_request_ok",
			incoming(crawlerRequest()), ""},
		{"sdk_crawler_request_without_client_rejected",
			incoming(func() *eventsv1.Event {
				e := crawlerRequest()
				e.Client = nil
				return e
			}()), "needs client and an http or https $url"},
		{"sdk_crawler_request_without_url_rejected",
			incoming(func() *eventsv1.Event {
				e := crawlerRequest()
				e.AutoProperties = nil
				return e
			}()), "needs client and an http or https $url"},
		{"sdk_crawler_request_relative_url_rejected",
			incoming(func() *eventsv1.Event {
				e := crawlerRequest()
				e.AutoProperties = propMap(map[string]string{"$url": "/pricing"})
				return e
			}()), "needs client and an http or https $url"},
		{"sdk_crawler_request_url_without_host_rejected",
			incoming(func() *eventsv1.Event {
				e := crawlerRequest()
				e.AutoProperties = propMap(map[string]string{"$url": "https://"})
				return e
			}()), "needs client and an http or https $url"},
		{"sdk_crawler_request_identity_rejected",
			incoming(func() *eventsv1.Event {
				e := crawlerRequest()
				e.DistinctId = proto.String("anon-u1")
				return e
			}()), "must not send distinct_id, session_id or cookieless"},
		{"sdk_cookieless_crawler_request_rejected",
			incoming(func() *eventsv1.Event {
				e := crawlerRequest()
				e.Cookieless = proto.Bool(true)
				return e
			}()), "must not send distinct_id, session_id or cookieless"},
		{"sdk_client_on_another_kind_rejected",
			incoming(func() *eventsv1.Event {
				e := validEvent()
				e.Client = crawlerRequest().GetClient()
				return e
			}()), "client is accepted only on a crawler_request event"},
		{"sdk_client_bad_ip_rejected",
			incoming(func() *eventsv1.Event {
				e := crawlerRequest()
				e.Client.Ip = proto.String("crawler")
				return e
			}()), "ip must be an IP address"},
		{"sdk_client_no_user_agent_rejected",
			incoming(func() *eventsv1.Event {
				e := crawlerRequest()
				e.Client.UserAgent = proto.String("")
				return e
			}()), "user_agent"},
		{"sdk_ordinary_ids_and_kinds_ok",
			incoming(func() *eventsv1.Event {
				e := validEvent()
				e.DistinctId = proto.String("my-crawler-account")
				e.Kind = proto.String("crawler_request_viewed")
				return e
			}()), ""},
		// The server fills a crawler_request's identity before the envelope re-validates it.
		{"envelope_crawler_request_ok",
			&eventsv1.EventBatch{ProjectId: proto.String("p1"),
				Events: []*eventsv1.Event{func() *eventsv1.Event {
					e := validEvent()
					e.DistinctId = proto.String(crawler.DistinctIDPrefix + "gptbot")
					e.Kind = proto.String(crawler.EventKind)
					return e
				}()}}, ""},
		{"sdk_bad_session_uuid_rejected",
			incoming(func() *eventsv1.Event {
				e := validEvent()
				e.SessionId = proto.String("not-a-uuid")
				return e
			}()), "session_id"},
		// The two-stage pin: the post-resolution shape must pass the internal
		// envelope but fail the SDK boundary — that asymmetry is the design.
		{"envelope_resolved_cookieless_ok",
			&eventsv1.EventBatch{ProjectId: proto.String("p1"),
				Events: []*eventsv1.Event{resolvedCookielessEvent()}}, ""},
		{"sdk_resolved_shape_rejected_at_boundary",
			incoming(resolvedCookielessEvent()),
			"must not send distinct_id"},
		{"envelope_normal_missing_identity_rejected",
			&eventsv1.EventBatch{ProjectId: proto.String("p1"),
				Events: []*eventsv1.Event{func() *eventsv1.Event {
					e := validEvent()
					e.DistinctId = nil
					return e
				}()}}, "required unless cookieless"},
		// A rule at the request level would fail the whole batch, so each of these
		// must be left to the handler.
		{"batch_leaves_event_rules_to_the_handler",
			&eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{
				func() *eventsv1.Event {
					e := validEvent()
					e.Kind = proto.String("Order Completed")
					return e
				}(),
				func() *eventsv1.Event {
					e := validEvent()
					e.DistinctId = proto.String("cookieless-fake")
					return e
				}(),
				func() *eventsv1.Event {
					e := cookielessEvent()
					e.DistinctId = proto.String("anon-u1")
					return e
				}(),
			}}, ""},
		{"batch_over_max_items_rejected",
			&eventsv1.BatchCreateRequest{Events: slices.Repeat([]*eventsv1.Event{validEvent()}, 1001)}, "1000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := protovalidate.Validate(tc.msg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want violation containing %q, got valid", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("violation = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}
