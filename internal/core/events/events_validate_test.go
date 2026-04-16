package events_test

import (
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/events/v1"
)

func strPtr(s string) *string { return &s }

// validEvent returns an Event with all required fields populated.
func validEvent() *eventsv1.Event {
	return &eventsv1.Event{
		EventId:    strPtr("550e8400-e29b-41d4-a716-446655440000"),
		DistinctId: strPtr("user-1"),
		Kind:       strPtr("page_view"),
		OccurTime:  timestamppb.Now(),
		SessionId:  strPtr("660e8400-e29b-41d4-a716-446655440001"),
	}
}

// TestEventValidation_KindNoReservedPrefix exercises the event.kind_no_reserved_prefix CEL rule.
func TestEventValidation_KindNoReservedPrefix(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		wantErr bool
	}{
		{name: "valid_kind", kind: "page_view", wantErr: false},
		{name: "reserved_prefix_rejected", kind: "cotton.signup", wantErr: true},
		{name: "prefix_not_substring", kind: "cottoncandy", wantErr: false},
		{name: "exact_prefix_rejected", kind: "cotton.", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validEvent()
			e.Kind = strPtr(tt.kind)
			err := protovalidate.Validate(e)
			if tt.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestEventValidation_AutoPropertiesDollarPrefix exercises the event.auto_properties_dollar_prefix CEL rule.
func TestEventValidation_AutoPropertiesDollarPrefix(t *testing.T) {
	tests := []struct {
		name           string
		autoProperties map[string]string
		wantErr        bool
	}{
		{name: "valid_dollar_prefix", autoProperties: map[string]string{"$browser": "Chrome", "$os": "macOS"}, wantErr: false},
		{name: "missing_dollar_prefix", autoProperties: map[string]string{"browser": "Chrome"}, wantErr: true},
		{name: "empty_map_accepted", autoProperties: map[string]string{}, wantErr: false},
		{name: "nil_map_accepted", autoProperties: nil, wantErr: false},
		{name: "mixed_valid_invalid", autoProperties: map[string]string{"$browser": "Chrome", "os": "macOS"}, wantErr: true},
		{name: "single_valid_key", autoProperties: map[string]string{"$device": "iPhone"}, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validEvent()
			e.AutoProperties = tt.autoProperties
			err := protovalidate.Validate(e)
			if tt.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}
