package clickhouse_test

import (
	"errors"
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
)

func opp(o commonv1.FilterOperator) *commonv1.FilterOperator { return &o }

// hasRule returns true if err is a *protovalidate.ValidationError and any of
// its violations has a rule id matching the given substring (e.g. "between_ordered_values"
// matches "property_filter.between_ordered_values"; "not_in" matches "enum.not_in").
//
// Falls back to substring match against err.Error() for the case where a CEL
// expression (e.g. `double(x)` on a non-numeric string) raises a runtime error
// — protovalidate wraps these as plain errors, not ValidationError, but the
// failing rule id appears in the message text.
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

// assertValid asserts that the given PropertyFilter passes proto validation.
func assertValid(t *testing.T, name string, f *commonv1.PropertyFilter) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		if err := protovalidate.Validate(f); err != nil {
			t.Errorf("expected valid, got error: %v", err)
		}
	})
}

// assertInvalid asserts that the given PropertyFilter fails proto validation.
// If expectedRule is supplied, one of the violations must have a matching rule id —
// guards against an unrelated CEL rule firing instead and silently masking a removed/renamed rule.
func assertInvalid(t *testing.T, name string, f *commonv1.PropertyFilter, expectedRule ...string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		err := protovalidate.Validate(f)
		if err == nil {
			t.Fatal("expected validation error, got nil")
		}
		if len(expectedRule) > 0 && !hasRule(err, expectedRule[0]) {
			t.Errorf("expected rule id %q among violations, got: %v", expectedRule[0], err)
		}
	})
}

// TestFilterOperatorValidation exercises the required + defined_only + not_in:[0] constraints on FilterOperator.
func TestFilterOperatorValidation(t *testing.T) {
	assertInvalid(t, "unspecified_rejected", &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_UNSPECIFIED),
		Value:    proto.String("US"),
	}, "not_in")
	assertInvalid(t, "undefined_value_rejected", &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: opp(999),
		Value:    proto.String("US"),
	}, "defined_only")
	assertValid(t, "valid_equals", &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_EQUALS),
		Value:    proto.String("US"),
	})
}

// TestPropertyFilterValidation exercises all CEL validation rules on PropertyFilter:
// value_required, numeric_value_required, values_required, values_not_allowed,
// between_requires_two_values, between_ordered_values, and value_not_allowed_for_set_operators.
func TestPropertyFilterValidation(t *testing.T) {
	// --- values_not_allowed regression: operators that use values[] must be accepted ---

	assertValid(t, "IN_with_values", &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_IN),
		Values:   []string{"US", "UK"},
	})
	assertValid(t, "NOT_IN_with_values", &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_IN),
		Values:   []string{"CN"},
	})
	assertValid(t, "BETWEEN_with_two_numeric_values", &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"10", "50"},
	})
	assertValid(t, "NOT_BETWEEN_with_two_numeric_values", &commonv1.PropertyFilter{
		Property: proto.String("amount"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{"0", "100"},
	})

	// --- values_not_allowed: operators that don't use values[] must reject non-empty values[] ---

	assertInvalid(t, "EQUALS_with_values_not_allowed", &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_EQUALS),
		Value:    proto.String("US"),
		Values:   []string{"extra"},
	}, "values_not_allowed")
	assertInvalid(t, "GTE_with_values_not_allowed", &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_GTE),
		Value:    proto.String("5"),
		Values:   []string{"1", "2"},
	}, "values_not_allowed")

	// --- value_required: operators that use values[] must not require a singular value ---

	assertValid(t, "BETWEEN_no_singular_value_required", &commonv1.PropertyFilter{
		Property: proto.String("price"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"1", "9"},
	})
	assertValid(t, "IN_no_singular_value_required", &commonv1.PropertyFilter{
		Property: proto.String("status"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_IN),
		Values:   []string{"active"},
	})

	// --- numeric_value_required: LTE/GTE/LT/GT use singular value, not values[] ---

	assertValid(t, "LTE_singular_numeric_value", &commonv1.PropertyFilter{
		Property: proto.String("age"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_LTE),
		Value:    proto.String("30"),
	})
	assertValid(t, "GTE_singular_numeric_value", &commonv1.PropertyFilter{
		Property: proto.String("age"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_GTE),
		Value:    proto.String("18"),
	})
	assertInvalid(t, "LTE_non_numeric_value_rejected", &commonv1.PropertyFilter{
		Property: proto.String("age"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_LTE),
		Value:    proto.String("not-a-number"),
	}, "numeric_value_required")
	assertInvalid(t, "BETWEEN_non_numeric_values_rejected", &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"abc", "50"},
	}, "numeric_value_required")

	// --- between_requires_two_values ---

	assertInvalid(t, "BETWEEN_requires_exactly_two_values", &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"10"},
	}, "between_requires_two_values")
	assertInvalid(t, "NOT_BETWEEN_requires_exactly_two_values", &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{},
	}, "between_requires_two_values")
	assertInvalid(t, "BETWEEN_three_values_rejected", &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"10", "50", "90"},
	}, "between_requires_two_values")
	assertInvalid(t, "NOT_BETWEEN_three_values_rejected", &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{"10", "50", "90"},
	}, "between_requires_two_values")

	// --- between_ordered_values: values[0] must be <= values[1] ---

	assertValid(t, "BETWEEN_equal_values_accepted", &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"50", "50"},
	})
	assertInvalid(t, "BETWEEN_reversed_values_rejected", &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"100", "10"},
	}, "between_ordered_values")
	assertInvalid(t, "NOT_BETWEEN_reversed_values_rejected", &commonv1.PropertyFilter{
		Property: proto.String("amount"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{"99.5", "1.5"},
	}, "between_ordered_values")
	assertValid(t, "NOT_BETWEEN_ordered_values_accepted", &commonv1.PropertyFilter{
		Property: proto.String("amount"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{"1.5", "99.5"},
	})

	// --- numeric_value_required: NOT_BETWEEN coverage ---

	assertInvalid(t, "NOT_BETWEEN_non_numeric_values_rejected", &commonv1.PropertyFilter{
		Property: proto.String("amount"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{"abc", "50"},
	}, "numeric_value_required")
	assertInvalid(t, "BETWEEN_second_value_non_numeric_rejected", &commonv1.PropertyFilter{
		Property: proto.String("score"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"50", "abc"},
	}, "numeric_value_required")
	assertInvalid(t, "NOT_BETWEEN_second_value_non_numeric_rejected", &commonv1.PropertyFilter{
		Property: proto.String("amount"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN),
		Values:   []string{"50", "abc"},
	}, "numeric_value_required")

	// --- value_not_allowed_for_set_operators ---

	assertValid(t, "IS_SET_no_value", &commonv1.PropertyFilter{
		Property: proto.String("email"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_IS_SET),
	})
	assertInvalid(t, "IS_SET_with_value_rejected", &commonv1.PropertyFilter{
		Property: proto.String("email"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_IS_SET),
		Value:    proto.String("something"),
	}, "value_not_allowed_for_set_operators")
	assertValid(t, "IS_NOT_SET_no_value", &commonv1.PropertyFilter{
		Property: proto.String("email"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET),
	})

	// --- value_required: scalar operators must reject empty value ---

	assertInvalid(t, "EQUALS_empty_value_rejected", &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_EQUALS),
		Value:    proto.String(""),
	}, "value_required")
	assertInvalid(t, "NOT_EQUALS_empty_value_rejected", &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS),
		Value:    proto.String(""),
	}, "value_required")
	assertInvalid(t, "CONTAINS_empty_value_rejected", &commonv1.PropertyFilter{
		Property: proto.String("email"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS),
		Value:    proto.String(""),
	}, "value_required")
	assertInvalid(t, "LTE_empty_value_rejected", &commonv1.PropertyFilter{
		Property: proto.String("age"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_LTE),
		Value:    proto.String(""),
	}, "value_required")

	// --- values_required: collection operators must reject empty values[] ---

	assertInvalid(t, "IN_empty_values_rejected", &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_IN),
		Values:   []string{},
	}, "values_required")
	assertInvalid(t, "NOT_IN_empty_values_rejected", &commonv1.PropertyFilter{
		Property: proto.String("$country"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_NOT_IN),
	}, "values_required")

	// --- between_ordered_values: negative and mixed-sign edges ---

	assertValid(t, "BETWEEN_negative_ordered_accepted", &commonv1.PropertyFilter{
		Property: proto.String("delta"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"-10", "-5"},
	})
	assertInvalid(t, "BETWEEN_negative_descending_rejected", &commonv1.PropertyFilter{
		Property: proto.String("delta"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"-5", "-10"},
	}, "between_ordered_values")
	assertValid(t, "BETWEEN_mixed_sign_ordered_accepted", &commonv1.PropertyFilter{
		Property: proto.String("delta"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"-5", "10"},
	})
	assertValid(t, "BETWEEN_zero_pair_accepted", &commonv1.PropertyFilter{
		Property: proto.String("delta"),
		Operator: opp(commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN),
		Values:   []string{"0", "0"},
	})
}
