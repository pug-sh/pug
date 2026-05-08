package profiles

import (
	"strings"
	"testing"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	profilesv1 "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1"
	"google.golang.org/protobuf/proto"
)

func TestBuildProfileFilterCondition_GroupOperators(t *testing.T) {
	cond, nextArg, err := buildProfileFilterCondition([]*profilesv1.FilterGroup{
		{
			Filters: []*commonv1.PropertyFilter{
				{
					Property: proto.String("plan"),
					Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
					Value:    proto.String("pro"),
				},
				{
					Property: proto.String("score"),
					Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE.Enum(),
					Value:    proto.String("10"),
				},
			},
		},
		{
			Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_OR.Enum(),
			Filters: []*commonv1.PropertyFilter{
				{
					Property: proto.String("role"),
					Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
					Value:    proto.String("admin"),
				},
				{
					Property: proto.String("tier"),
					Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
					Value:    proto.String("gold"),
				},
			},
		},
	}, commonv1.LogicalOperator_LOGICAL_OPERATOR_OR, 5)
	if err != nil {
		t.Fatalf("buildProfileFilterCondition: %v", err)
	}
	if nextArg != 9 {
		t.Fatalf("nextArg = %d, want 9", nextArg)
	}
	if got, want := len(cond.args), 4; got != want {
		t.Fatalf("len(args) = %d, want %d", got, want)
	}
	if !strings.Contains(cond.sql, " OR ") {
		t.Fatalf("sql = %q, want OR between groups", cond.sql)
	}
	if !strings.Contains(cond.sql, " AND ") {
		t.Fatalf("sql = %q, want AND within default group", cond.sql)
	}
}

func TestBuildPropertyFilterCondition_RejectsNonProfileSource(t *testing.T) {
	_, _, err := buildPropertyFilterCondition(&commonv1.PropertyFilter{
		Property: proto.String("plan"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
		Value:    proto.String("pro"),
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_AUTO.Enum(),
	}, 1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported profile filter source") {
		t.Fatalf("err = %v, want unsupported source", err)
	}
}

func TestBuildPropertyFilterCondition_BooleanEqualsUsesTextComparison(t *testing.T) {
	cond, nextArg, err := buildPropertyFilterCondition(&commonv1.PropertyFilter{
		Property: proto.String("subscribed"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
		Value:    proto.String("true"),
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
	}, 3)
	if err != nil {
		t.Fatalf("buildPropertyFilterCondition: %v", err)
	}
	if nextArg != 4 {
		t.Fatalf("nextArg = %d, want 4", nextArg)
	}
	if cond.sql != "coalesce(properties->>'subscribed', '') = $3" {
		t.Fatalf("sql = %q", cond.sql)
	}
	if len(cond.args) != 1 || cond.args[0] != "true" {
		t.Fatalf("args = %#v, want [true]", cond.args)
	}
}
