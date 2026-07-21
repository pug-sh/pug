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

// TestExcludeCookielessForAgg_IsExhaustive is the enforcement behind
// excludeCookielessForAgg's switch: it ranges over every AggregationType the
// proto defines and requires an explicit decision for each.
//
// The helper previously used a two-member inclusion list, which failed open — a
// new aggregation returned false and silently admitted cookieless ids into a
// metric that might count people. No other layer catches that: protovalidate's
// enum.defined_only accepts any defined member, and both rollupAggExpr and
// aggregationExpr have silent defaults. This test is the only thing that turns
// "someone added a metric and didn't think about cookieless" into a red build.
func TestExcludeCookielessForAgg_IsExhaustive(t *testing.T) {
	// Every member must appear here with the decision it was given. Adding a
	// proto member without adding it below fails the completeness check.
	decided := map[insightsv1.AggregationType]bool{
		insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED:  false,
		insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL:        false,
		insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS: true,
		insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG: true,
		insightsv1.AggregationType_AGGREGATION_TYPE_SUM:          false,
		insightsv1.AggregationType_AGGREGATION_TYPE_AVG:          false,
		insightsv1.AggregationType_AGGREGATION_TYPE_MIN:          false,
		insightsv1.AggregationType_AGGREGATION_TYPE_MAX:          false,
	}

	spec := &insightsv1.InsightQuerySpec{}
	for num, name := range insightsv1.AggregationType_name {
		agg := insightsv1.AggregationType(num)
		want, ok := decided[agg]
		if !ok {
			t.Errorf("%s has no cookieless decision: add it to excludeCookielessForAgg's switch AND to this table — does it count people?", name)
			continue
		}
		if got := excludeCookielessForAgg(spec, agg); got != want {
			t.Errorf("excludeCookielessForAgg(%s) = %v, want %v", name, got, want)
		}
	}

	// The toggle must re-admit cookieless ids for every aggregation, including
	// the ones that exclude by default.
	on := &insightsv1.InsightQuerySpec{IncludeCookieless: proto.Bool(true)}
	for num, name := range insightsv1.AggregationType_name {
		if excludeCookielessForAgg(on, insightsv1.AggregationType(num)) {
			t.Errorf("include_cookieless=true must admit cookieless ids for %s", name)
		}
	}
}
