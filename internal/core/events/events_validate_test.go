package events_test

import (
	"errors"
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/events/v1"
)

// hasRule returns true if err is a *protovalidate.ValidationError and any of
// its violations has a rule id matching the given substring. Falls back to
// substring match against err.Error() for CEL runtime errors (e.g. failed type
// coercion), which protovalidate wraps as plain errors but include the rule id
// in the message text.
func hasRule(err error, ruleSubstring string) bool {
	var ve *protovalidate.ValidationError
	if errors.As(err, &ve) {
		for _, v := range ve.Violations {
			if strings.Contains(v.Proto.GetRuleId(), ruleSubstring) {
				return true
			}
		}
		return false
	}
	return strings.Contains(err.Error(), ruleSubstring)
}

// validEvent returns an Event with all required fields populated.
func validEvent() *eventsv1.Event {
	return &eventsv1.Event{
		EventId:    proto.String("550e8400-e29b-41d4-a716-446655440000"),
		DistinctId: proto.String("user-1"),
		Kind:       proto.String("page_view"),
		OccurTime:  timestamppb.Now(),
		SessionId:  proto.String("660e8400-e29b-41d4-a716-446655440001"),
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
		// Case-sensitivity: startsWith is byte-exact, so upper/mixed-case variants must be accepted.
		{name: "case_sensitive_upper_accepted", kind: "Cotton.foo", wantErr: false},
		{name: "case_sensitive_shouting_accepted", kind: "COTTON.foo", wantErr: false},
		{name: "case_sensitive_mixed_accepted", kind: "CoTtOn.abc", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validEvent()
			e.Kind = proto.String(tt.kind)
			err := protovalidate.Validate(e)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if !hasRule(err, "kind_no_reserved_prefix") {
					t.Errorf("expected rule kind_no_reserved_prefix in violations, got: %v", err)
				}
				return
			}
			if err != nil {
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
		// Edge cases: protobuf map keys can be empty strings, and the literal single-char "$" key
		// is valid per startsWith("$") semantics.
		{name: "single_dollar_char_key_accepted", autoProperties: map[string]string{"$": "value"}, wantErr: false},
		{name: "empty_string_key_rejected", autoProperties: map[string]string{"": "value"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validEvent()
			e.AutoProperties = tt.autoProperties
			err := protovalidate.Validate(e)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if !hasRule(err, "auto_properties_dollar_prefix") {
					t.Errorf("expected rule auto_properties_dollar_prefix in violations, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}
