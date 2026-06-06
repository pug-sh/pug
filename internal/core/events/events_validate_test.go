package events_test

import (
	"errors"
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
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

// TestEventValidation_KindPattern exercises the kind string.pattern constraint (aligned with common.v1.EventFilter).
func TestEventValidation_KindPattern(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		wantErr bool
	}{
		{name: "snake_case", kind: "page_view", wantErr: false},
		{name: "hyphenated", kind: "sign-up", wantErr: false},
		{name: "dotted", kind: "com.example.event", wantErr: false},
		{name: "space_rejected", kind: "hello world", wantErr: true},
		{name: "slash_rejected", kind: "foo/bar", wantErr: true},
		{name: "unicode_rejected", kind: "café", wantErr: true},
		{name: "empty_rejected", kind: "", wantErr: true},
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
		autoProperties map[string]*commonv1.PropertyValue
		wantErr        bool
	}{
		{name: "valid_dollar_prefix", autoProperties: propMap("$browser", "$os"), wantErr: false},
		{name: "missing_dollar_prefix", autoProperties: propMap("browser"), wantErr: true},
		{name: "empty_map_accepted", autoProperties: map[string]*commonv1.PropertyValue{}, wantErr: false},
		{name: "nil_map_accepted", autoProperties: nil, wantErr: false},
		{name: "mixed_valid_invalid", autoProperties: propMap("$browser", "os"), wantErr: true},
		{name: "single_valid_key", autoProperties: propMap("$device"), wantErr: false},
		// Edge cases: protobuf map keys can be empty strings, and the literal single-char "$" key
		// is valid per startsWith("$") semantics.
		{name: "single_dollar_char_key_accepted", autoProperties: propMap("$"), wantErr: false},
		{name: "empty_string_key_rejected", autoProperties: propMap(""), wantErr: true},
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

func propMap(keys ...string) map[string]*commonv1.PropertyValue {
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]*commonv1.PropertyValue, len(keys))
	for _, key := range keys {
		out[key] = &commonv1.PropertyValue{
			Value: &commonv1.PropertyValue_StringValue{StringValue: "value"},
		}
	}
	return out
}
