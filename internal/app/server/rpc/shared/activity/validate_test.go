package activity

import (
	"errors"
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	activityv1 "github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1"
)

// An auto property key always carries the "$" prefix. A bare one used to pass
// validation and fail in the query builder, which the handler reported as an
// internal error, so a client typo looked like a server fault.
func TestGetPropertyValuesRequest_AutoKeyRequiresDollarPrefix(t *testing.T) {
	const rule = "get_property_values_request.auto_key_dollar_prefix"

	tests := []struct {
		name   string
		key    string
		source commonv1.PropertySource
		ok     bool
	}{
		{"auto key without prefix is rejected", "browser", commonv1.PropertySource_PROPERTY_SOURCE_AUTO, false},
		{"auto key with prefix is accepted", "$browser", commonv1.PropertySource_PROPERTY_SOURCE_AUTO, true},
		{"custom key without prefix is accepted", "plan", commonv1.PropertySource_PROPERTY_SOURCE_CUSTOM, true},
		{"profile key without prefix is accepted", "plan", commonv1.PropertySource_PROPERTY_SOURCE_PROFILE, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := protovalidate.Validate(&activityv1.GetPropertyValuesRequest{
				PropertyKey: proto.String(tt.key),
				Source:      tt.source.Enum(),
			})
			if tt.ok {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			ve, isValidation := errors.AsType[*protovalidate.ValidationError](err)
			if !isValidation {
				t.Fatalf("expected a validation error, got %v", err)
			}
			for _, v := range ve.Violations {
				if v.Proto.GetRuleId() == rule {
					return
				}
			}
			t.Fatalf("expected rule %q to fail, got %v", rule, err)
		})
	}
}

// required only checks presence under editions, so an explicit UNSPECIFIED needs
// not_in: without it the request reached the service's default case as internal.
func TestGetPropertyValuesRequest_RejectsUnspecifiedSource(t *testing.T) {
	err := protovalidate.Validate(&activityv1.GetPropertyValuesRequest{
		PropertyKey: proto.String("plan"),
		Source:      commonv1.PropertySource_PROPERTY_SOURCE_UNSPECIFIED.Enum(),
	})
	ve, ok := errors.AsType[*protovalidate.ValidationError](err)
	if !ok || len(ve.Violations) != 1 || ve.Violations[0].Proto.GetRuleId() != "enum.not_in" {
		t.Fatalf("got %v, want only an enum.not_in violation", err)
	}
}
