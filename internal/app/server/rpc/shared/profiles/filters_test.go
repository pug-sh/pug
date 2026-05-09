package profiles

import (
	"testing"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
)

func TestProfileAutoPropertySummaryExprs_CoalescesNullableColumns(t *testing.T) {
	tests := map[string]string{
		"$browser":        "coalesce(activity_summary.latest_browser, '')",
		"$browserVersion": "coalesce(activity_summary.latest_browser_version, '')",
		"$os":             "coalesce(activity_summary.latest_os, '')",
		"$osVersion":      "coalesce(activity_summary.latest_os_version, '')",
		"$device":         "coalesce(activity_summary.latest_device, '')",
		"$country":        "coalesce(activity_summary.latest_country, '')",
		"$region":         "coalesce(activity_summary.latest_region, '')",
		"$city":           "coalesce(activity_summary.latest_city, '')",
	}

	for property, want := range tests {
		got, numeric, err := profileAutoPropertySummaryExprs(property)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", property, err)
		}
		if got != want {
			t.Fatalf("%s: expr = %q, want %q", property, got, want)
		}
		if numeric != "" {
			t.Fatalf("%s: numeric expr = %q, want empty", property, numeric)
		}
	}
}

func TestBuildSingleProfileFilterCondition_AutoIsNotSetUsesCoalescedExpr(t *testing.T) {
	cond, err := buildSingleProfileFilterCondition(&commonv1.PropertyFilter{
		Property: stringp("$browser"),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET.Enum(),
		Source:   commonv1.PropertySource_PROPERTY_SOURCE_AUTO.Enum(),
	})
	if err != nil {
		t.Fatalf("buildSingleProfileFilterCondition: %v", err)
	}
	if got, want := cond.SQL(), "coalesce(activity_summary.latest_browser, '') = ''"; got != want {
		t.Fatalf("SQL = %q, want %q", got, want)
	}
	if args := cond.Args(); len(args) != 0 {
		t.Fatalf("Args = %v, want none", args)
	}
}

func stringp(s string) *string { return &s }
