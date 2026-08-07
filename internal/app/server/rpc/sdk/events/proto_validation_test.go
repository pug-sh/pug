package events

import (
	"strings"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

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

func TestEventValidation_TwoStageContract(t *testing.T) {
	cases := []struct {
		name    string
		msg     proto.Message
		wantErr string // substring of the violation id/message; "" = must pass
	}{
		{"sdk_normal_event_ok",
			&eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{validEvent()}}, ""},
		{"sdk_cookieless_empty_identity_ok",
			&eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{cookielessEvent()}}, ""},
		{"sdk_normal_missing_distinct_id_rejected",
			&eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{func() *eventsv1.Event {
				e := validEvent()
				e.DistinctId = nil
				return e
			}()}}, "required unless cookieless"},
		{"sdk_cookieless_with_distinct_id_rejected",
			&eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{func() *eventsv1.Event {
				e := cookielessEvent()
				e.DistinctId = proto.String("anon-u1")
				return e
			}()}}, "must not send distinct_id"},
		{"sdk_cookieless_with_session_id_rejected",
			&eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{func() *eventsv1.Event {
				e := cookielessEvent()
				e.SessionId = proto.String("f47ac10b-58cc-4372-a567-0e02b2c3d483")
				return e
			}()}}, "must not send distinct_id"},
		{"sdk_spoofed_prefix_rejected",
			&eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{func() *eventsv1.Event {
				e := validEvent()
				e.DistinctId = proto.String("cookieless-fake")
				return e
			}()}}, "reserved 'cookieless-' prefix"},
		{"sdk_bad_session_uuid_rejected",
			&eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{func() *eventsv1.Event {
				e := validEvent()
				e.SessionId = proto.String("not-a-uuid")
				return e
			}()}}, "session_id"},
		// The two-stage pin: the post-resolution shape must pass the internal
		// envelope but fail the SDK boundary — that asymmetry is the design.
		{"envelope_resolved_cookieless_ok",
			&eventsv1.EventBatch{ProjectId: proto.String("p1"),
				Events: []*eventsv1.Event{resolvedCookielessEvent()}}, ""},
		{"sdk_resolved_shape_rejected_at_boundary",
			&eventsv1.BatchCreateRequest{Events: []*eventsv1.Event{resolvedCookielessEvent()}},
			"must not send distinct_id"},
		{"envelope_normal_missing_identity_rejected",
			&eventsv1.EventBatch{ProjectId: proto.String("p1"),
				Events: []*eventsv1.Event{func() *eventsv1.Event {
					e := validEvent()
					e.DistinctId = nil
					return e
				}()}}, "required unless cookieless"},
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
