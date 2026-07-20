package insights

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func TestExcludeCookielessForAgg(t *testing.T) {
	spec := &insightsv1.InsightQuerySpec{}
	inc := &insightsv1.InsightQuerySpec{IncludeCookieless: proto.Bool(true)}
	cases := []struct {
		name string
		spec *insightsv1.InsightQuerySpec
		agg  insightsv1.AggregationType
		want bool
	}{
		{"unique_users_default_excludes", spec, insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS, true},
		{"per_user_avg_default_excludes", spec, insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG, true},
		{"total_never_excludes", spec, insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, false},
		{"unspecified_defaults_to_total", spec, insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED, false},
		{"sum_never_excludes", spec, insightsv1.AggregationType_AGGREGATION_TYPE_SUM, false},
		{"toggle_includes_unique_users", inc, insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS, false},
		{"toggle_includes_per_user_avg", inc, insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := excludeCookielessForAgg(tc.spec, tc.agg); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExcludeCookielessForPersons(t *testing.T) {
	if !excludeCookielessForPersons(&insightsv1.InsightQuerySpec{}) {
		t.Error("person-based insights must exclude by default")
	}
	if excludeCookielessForPersons(&insightsv1.InsightQuerySpec{IncludeCookieless: proto.Bool(true)}) {
		t.Error("toggle must lift person-based exclusion")
	}
}

func TestCookielessExclusionCond(t *testing.T) {
	if c := cookielessExclusionCond(false, ""); !c.IsZero() {
		t.Errorf("no-exclusion must be the zero condition (skipped by Where), got %q", c.SQL())
	}
	if sql := cookielessExclusionCond(true, "").SQL(); !strings.Contains(sql, "NOT startsWith(distinct_id, 'cookieless-')") {
		t.Errorf("unaliased cond = %q", sql)
	}
	if sql := cookielessExclusionCond(true, "e").SQL(); !strings.Contains(sql, "NOT startsWith(e.distinct_id, 'cookieless-')") {
		t.Errorf("aliased cond = %q", sql)
	}
}
